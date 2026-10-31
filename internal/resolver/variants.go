package resolver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var _ core.VariantResolver = (*Service)(nil)

// maxVariants caps the Play Version picker (core.MaxVariants): Auto, up to
// maxAlternativesLimit same-quality alternatives, and up to maxTierVariants
// distinct tiers and cuts.
const (
	maxVariants          = core.MaxVariants
	maxAlternativesLimit = 3
	maxTierVariants      = 4 // Auto plus three tiers, the picker before alternatives
)

// Variants implements core.VariantResolver. It reads only the cached
// search results — rendering a detail page never spends indexer budget.
// When nothing is cached it returns (nil, nil) and the caller renders the
// single Auto media; the detail page's PreResolve populates the cache, so
// a later load shows versions.
func (s *Service) Variants(ctx context.Context, req core.ResolveRequest, max int) ([]core.ReleaseVariant, error) {
	if req.Title == nil {
		return nil, fmt.Errorf("resolver: nil title")
	}
	if max <= 0 || max > maxVariants {
		max = maxVariants
	}
	req.Client = ApplyClientRules(s.qualityCfg(), req.Client, req.User)
	profile := s.ProfileFor(req.Client, req.User)
	sreq := searchRequestFor(req.Title)
	results, ok := s.peekSearchCache(searchCacheKey(sreq))
	if !ok || len(results) == 0 {
		return nil, nil
	}
	// An owner pin that applies to this client is what normal resolution
	// tries first, so it is Auto (pins.go).
	pinID := ""
	if pin, _ := s.pinForResolve(req); pin != nil {
		pinID = pin.Release.ID
		results = withRelease(results, pin.Release)
	}
	// Auto is exactly what normal resolution would pick: strict profile.
	strict, _, err := s.rankCandidates(ctx, req, sreq, profile, results, nil, pinID)
	if err != nil {
		return nil, err
	}
	if len(strict) == 0 {
		return nil, nil
	}
	strict = pinFirst(strict, pinID)
	pool := strict
	// Taller tiers (a 4K copy on a 1080p profile) are listed only when the
	// owner opts in. The relaxed pool still stops at the client's own
	// limits, and HDR policy and size limits stay on the strict profile.
	// Without the opt-in, Resolve also refuses a Play Version pick above
	// that ceiling (resolver.go).
	allowTaller := s.resolverCfg().OfferHigherVersions
	if allowTaller {
		if relaxed, ok := relaxedVariantProfile(profile, req.Client); ok {
			pool, _, err = s.rankCandidates(ctx, req, sreq, relaxed, results, nil, pinID)
			if err != nil {
				return nil, err
			}
		}
	}
	vs := pickVariants(req.Title, req.Client, profile, strict[0].rel, pool, strict, s.sameQualityAlternatives(), max, allowTaller)
	labelPlayedAuto(vs, req.PlayedRelease, results, s.LookupRelease)
	return vs, nil
}

// labelPlayedAuto names Auto after the release this viewer actually played
// (the one a resume opens) when that is not one of the explicit versions:
// Auto resolved past its first candidate, or an earlier pick ranks
// differently now. The played release is looked up in the cached results,
// then among recently seen releases; unknown leaves Auto's own label.
func labelPlayedAuto(vs []core.ReleaseVariant, played string, results []core.Release, lookup func(string) (core.Release, bool)) {
	if played == "" || len(vs) == 0 || !vs[0].Auto {
		return
	}
	for _, v := range vs {
		if v.Release.ID == played {
			return // Auto itself, or an explicit version the client marks as played
		}
	}
	for _, r := range results {
		if r.ID == played {
			vs[0].Label = autoLabel(r)
			return
		}
	}
	if r, ok := lookup(played); ok {
		vs[0].Label = autoLabel(r)
	}
}

// relaxedVariantProfile raises the profile's height cap to the client's
// own maximum so taller tiers can be offered as explicit picks. HDR
// policy and size limits are left as the owner set them. It reports
// false when nothing changes. Callers use it only when the owner has
// opted in to offering higher versions.
func relaxedVariantProfile(profile core.QualityProfile, caps core.ClientCaps) (core.QualityProfile, bool) {
	clientMax := caps.MaxHeight
	if clientMax <= 0 {
		clientMax = 2160
	}
	if profile.MaxHeight <= 0 || profile.MaxHeight >= clientMax {
		return profile, false
	}
	p := profile
	p.MaxHeight = clientMax
	return p, true
}

// sameQualityAlternatives is the owner's setting, clamped to 0..3.
func (s *Service) sameQualityAlternatives() int {
	n := s.resolverCfg().SameQualityAlternatives
	if n < 0 {
		return 0
	}
	if n > maxAlternativesLimit {
		return maxAlternativesLimit
	}
	return n
}

func (s *Service) peekSearchCache(key string) ([]core.Release, bool) {
	s.mu.RLock()
	e, ok := s.searchCache[key]
	s.mu.RUnlock()
	if !ok || !s.now().Before(e.expires) {
		return nil, false
	}
	return append([]core.Release(nil), e.results...), true
}

// qualityTier buckets a release for deduplication: height plus HDR kind.
func qualityTier(info core.ReleaseInfo) string {
	kind := "sdr"
	switch {
	case info.DolbyVision:
		kind = "dv"
	case info.HDR10:
		kind = "hdr"
	}
	return strconv.Itoa(info.Height) + "-" + kind
}

func variantKey(info core.ReleaseInfo) string {
	return info.Edition + "|" + qualityTier(info)
}

// pickVariants assembles up to max distinct versions: Auto first, then up to
// the configured number of other releases of Auto's own cut, resolution and HDR type,
// then a 4K HDR/DV tier, a high-compatibility 1080p tier, one entry per
// distinct cut, and any remaining distinct tiers. Tiers are best-scored
// first within each slot and deduplicated by (edition, quality tier); the
// alternatives come from the strict profile pool, in rank order.
func pickVariants(t *core.Title, caps core.ClientCaps, profile core.QualityProfile, auto core.Release, pool, strict []candidate, alternatives, max int, allowTaller bool) []core.ReleaseVariant {
	usedRel := map[string]bool{}
	usedKey := map[string]bool{}
	var out []core.ReleaseVariant
	tierMax := max
	if tierMax > maxTierVariants {
		tierMax = maxTierVariants
	}
	variant := func(rel core.Release, label string, isAuto bool) core.ReleaseVariant {
		return core.ReleaseVariant{
			ID:      rel.ID,
			Label:   label,
			Edition: rel.Info.Edition,
			Auto:    isAuto,
			Release: rel,
			Info:    estimateVariantInfo(t, sizedForEpisode(rel)),
		}
	}
	add := func(rel core.Release, kind string, isAuto bool) bool {
		if !isAuto && releaseAboveProfile(rel, t, profile, allowTaller) {
			return false
		}
		if len(out) >= tierMax || usedRel[rel.ID] || usedKey[variantKey(rel.Info)] {
			return false
		}
		usedRel[rel.ID] = true
		usedKey[variantKey(rel.Info)] = true
		out = append(out, variant(rel, variantLabel(rel, kind), isAuto))
		return true
	}
	add(auto, "auto", true)

	// 4K HDR / Dolby Vision, only when this client can direct-play it.
	if canOffer4KHDR(caps, profile) {
		for _, c := range pool {
			if c.rel.Info.Height >= 2160 && (c.rel.Info.HDR10 || c.rel.Info.DolbyVision) && add(c.rel, "4k", false) {
				break
			}
		}
	}
	// High-compatibility tier (most-compatible codec/audio, SDR).
	if rel, ok := bestCompat(pool, caps, usedRel); ok {
		add(rel, "compat", false)
	}
	// One entry per distinct cut, best first.
	seenEdition := map[string]bool{auto.Info.Edition: true}
	for _, c := range pool {
		ed := c.rel.Info.Edition
		if ed == "" || seenEdition[ed] || usedRel[c.rel.ID] {
			continue
		}
		seenEdition[ed] = true
		add(c.rel, "edition", false)
	}
	// Remaining distinct tiers fill any open slots.
	for _, c := range pool {
		add(c.rel, "tier", false)
	}

	// Same-quality alternatives sit right after Auto, so plain Play and the
	// first menu entry stay on Auto. They share Auto's variantKey, so none
	// of them can be a tier above.
	alts := pickAlternatives(auto, strict, alternatives, max-len(out))
	if len(alts) > 0 {
		kept := alts[:0]
		for _, rel := range alts {
			if releaseAboveProfile(rel, t, profile, false) {
				continue
			}
			kept = append(kept, rel)
		}
		alts = kept
	}
	if len(alts) == 0 {
		return out
	}
	withAlts := make([]core.ReleaseVariant, 0, len(out)+len(alts))
	withAlts = append(withAlts, out[:1]...)
	for _, rel := range alts {
		withAlts = append(withAlts, variant(rel, variantLabel(rel, "alt"), false))
	}
	return append(withAlts, out[1:]...)
}

// pickAlternatives returns up to limit releases from the ranked pool
// that share Auto's cut, resolution and HDR type, skipping Auto itself and
// any release whose name and size repeat one already chosen (the same file
// on another indexer or source).
func pickAlternatives(auto core.Release, ranked []candidate, limit, room int) []core.Release {
	if room < limit {
		limit = room
	}
	if limit <= 0 {
		return nil
	}
	seen := map[string]bool{releaseIdentity(auto): true}
	key := variantKey(auto.Info)
	var out []core.Release
	for _, c := range ranked {
		if len(out) >= limit {
			break
		}
		if c.rel.ID == auto.ID || variantKey(c.rel.Info) != key || seen[releaseIdentity(c.rel)] {
			continue
		}
		seen[releaseIdentity(c.rel)] = true
		out = append(out, c.rel)
	}
	return out
}

// releaseIdentity names a release for duplicate detection: the release
// name (case-insensitive) and its size.
func releaseIdentity(rel core.Release) string {
	return strings.ToLower(strings.TrimSpace(rel.Name)) + "|" + strconv.FormatInt(rel.Size, 10)
}

// releaseAboveProfile reports a release Play Version must not list.
// Height is waived only when the owner opted in. HDR policy and the
// size ceiling always stand, including for that opt-in. Auto is never
// passed here.
func releaseAboveProfile(rel core.Release, t *core.Title, profile core.QualityProfile, allowTaller bool) bool {
	info := rel.Info
	if !allowTaller && profile.MaxHeight > 0 && info.Height > profile.MaxHeight {
		return true
	}
	if info.DolbyVision && !profile.AllowDolbyVision && !(info.HDR10 && profile.AllowHDR) {
		return true
	}
	if info.HDR10 && !info.DolbyVision && !profile.AllowHDR {
		return true
	}
	size := sizedForEpisode(rel).Size
	if size > 0 && t != nil && t.Runtime > 0 {
		mbPerMin := float64(size) / (1 << 20) / t.Runtime.Minutes()
		_, cfgMax := profile.SizeBoundsFor(t)
		if maxMBPerMin := effectiveMaxSizeMBPerMin(cfgMax, info); maxMBPerMin > 0 && mbPerMin > maxMBPerMin {
			return true
		}
	}
	return false
}

func canOffer4KHDR(caps core.ClientCaps, profile core.QualityProfile) bool {
	if caps.MaxHeight > 0 && caps.MaxHeight < 2160 {
		return false
	}
	// profile.AllowHDR/AllowDolbyVision are already clamped to the
	// client's HDR/DV support by SelectProfile.
	return profile.AllowHDR || profile.AllowDolbyVision
}

// bestCompat picks the most compatible release at (or below) the client's
// comfort height: SDR, the most widely playable video codec the client
// supports (H.264 preferred), AAC/AC3/E-AC-3 audio, WEB-DL preferred.
func bestCompat(pool []candidate, caps core.ClientCaps, used map[string]bool) (core.Release, bool) {
	target := 1080
	if caps.MaxHeight > 0 && caps.MaxHeight < target {
		target = caps.MaxHeight
	}
	var best core.Release
	bestScore := -1e18
	found := false
	for _, c := range pool {
		rel := c.rel
		if used[rel.ID] || rel.Info.Height <= 0 || rel.Info.Height > target || rel.Info.Height < 720 {
			continue
		}
		if rel.Info.HDR10 || rel.Info.DolbyVision {
			continue
		}
		sc := rel.Score / 10 // overall rank as tiebreak
		if rel.Info.Height == target {
			sc += 100
		} else if rel.Info.Height == 720 {
			sc += 60
		}
		switch rel.Info.VideoCodec {
		case "h264":
			sc += 50
		case "":
			sc += 5
		default:
			sc += 20
		}
		switch rel.Info.AudioCodec {
		case "aac", "ac3", "eac3":
			sc += 30
		case "truehd", "dtshd", "dts", "flac":
			sc -= 20
		}
		switch rel.Info.Source {
		case core.SourceWEBDL:
			sc += 20
		case core.SourceWEBRip:
			sc += 10
		case core.SourceBluRay:
			sc += 5
		}
		if !found || sc > bestScore {
			best, bestScore, found = rel, sc, true
		}
	}
	return best, found
}

// variantLabel is the viewer-facing version name (Plex Media@title,
// Jellyfin and Emby MediaSources[].Name, Silo and Stremio version names):
// what the release is, with its source and group when the name says, e.g.
// "Auto · 1080p WEB-DL · DD+ 5.1 · FLUX", "4K Dolby Vision Remux ·
// FraMeSToR" or "Director's Cut — 1080p Blu-ray · AMIABLE".
func variantLabel(rel core.Release, kind string) string {
	group := strings.TrimSpace(rel.Info.Group)
	withGroup := func(s string) string {
		if group == "" {
			return s
		}
		return s + " · " + group
	}
	switch kind {
	case "auto":
		return autoLabel(rel)
	case "compat":
		return withGroup(qualityPhrase(rel.Info)) + " (High Compatibility)"
	case "edition":
		return rel.Info.Edition + " — " + withGroup(qualityPhrase(rel.Info))
	case "alt":
		return alternativeLabel(rel)
	}
	q := qualityPhrase(rel.Info)
	if rel.Info.Edition != "" {
		q += " · " + rel.Info.Edition
	}
	return withGroup(q)
}

// autoLabel names the release Auto plays, kept short (no size):
// "Auto · 1080p WEB-DL · DD+ 5.1 · FLUX".
func autoLabel(rel core.Release) string {
	parts := []string{"Auto", qualityPhrase(rel.Info)}
	if a := audioPhrase(rel.Info); a != "" {
		parts = append(parts, a)
	}
	if g := strings.TrimSpace(rel.Info.Group); g != "" {
		parts = append(parts, g)
	}
	return strings.Join(parts, " · ")
}

// alternativeLabel tells same-quality releases apart: quality, audio, size
// and release group, e.g. "1080p WEB-DL · DD+ 5.1 · 2.3 GB · FLUX".
// Anything the release name does not say is left out.
func alternativeLabel(rel core.Release) string {
	parts := []string{qualityPhrase(rel.Info)}
	if a := audioPhrase(rel.Info); a != "" {
		parts = append(parts, a)
	}
	if rel.Size > 0 {
		parts = append(parts, sizePhrase(rel.Size))
	}
	if g := strings.TrimSpace(rel.Info.Group); g != "" {
		parts = append(parts, g)
	}
	return strings.Join(parts, " · ")
}

func audioPhrase(info core.ReleaseInfo) string {
	codec := map[string]string{
		"aac": "AAC", "ac3": "DD", "eac3": "DD+", "truehd": "TrueHD",
		"dts": "DTS", "dtshd": "DTS-HD", "flac": "FLAC", "opus": "Opus", "mp3": "MP3",
	}[info.AudioCodec]
	if codec == "" {
		codec = strings.ToUpper(info.AudioCodec)
	}
	parts := []string{}
	if codec != "" {
		parts = append(parts, codec)
	}
	if info.Atmos {
		parts = append(parts, "Atmos")
	}
	if info.Channels != "" {
		parts = append(parts, info.Channels)
	}
	return strings.Join(parts, " ")
}

func sizePhrase(size int64) string {
	const gb = 1 << 30
	if size >= gb {
		return strconv.FormatFloat(float64(size)/gb, 'f', 1, 64) + " GB"
	}
	return strconv.FormatInt((size+(1<<20)/2)>>20, 10) + " MB"
}

func qualityPhrase(info core.ReleaseInfo) string {
	res := "SD"
	switch {
	case info.Height >= 2160:
		res = "4K"
	case info.Height > 0:
		res = strconv.Itoa(info.Height) + "p"
	}
	parts := []string{res}
	switch {
	case info.DolbyVision:
		parts = append(parts, "Dolby Vision")
	case info.HDR10:
		parts = append(parts, "HDR10")
	}
	if src := sourceWord(info.Source); src != "" {
		parts = append(parts, src)
	}
	return strings.Join(parts, " ")
}

func sourceWord(s core.Source) string {
	switch s {
	case core.SourceRemux:
		return "Remux"
	case core.SourceWEBDL:
		return "WEB-DL"
	case core.SourceBluRay:
		return "Blu-ray"
	case core.SourceWEBRip:
		return "WEBRip"
	case core.SourceHDTV:
		return "HDTV"
	case core.SourceDVD:
		return "DVD"
	case core.SourceScreener:
		return "Screener"
	case core.SourceCam:
		return "CAM"
	}
	return ""
}

// estimateVariantInfo derives the expected MediaInfo from a parsed release
// name: resolution, codecs, HDR/DV, channels, and a bitrate from the
// release size over the title runtime (typical bitrates when unknown).
func estimateVariantInfo(t *core.Title, rel core.Release) core.MediaInfo {
	info := rel.Info
	height := info.Height
	if height <= 0 {
		height = 1080
	}
	codec := info.VideoCodec
	if codec == "" {
		codec = "h264"
		if height >= 2160 {
			codec = "hevc"
		}
	}
	video := core.StreamInfo{
		Index: 0, Type: "video", Codec: codec,
		Width: variantWidth(height), Height: height,
		FrameRate: 23.976, BitDepth: 8, Default: true,
	}
	if codec == "h264" {
		video.Profile = "high"
	} else {
		video.Profile = "main"
	}
	if info.HDR10 || info.DolbyVision {
		video.BitDepth = 10
		video.ColorTransfer = "smpte2084"
		if codec == "hevc" {
			video.Profile = "main 10"
		}
	}
	video.DolbyVision = info.DolbyVision

	audioCodec := info.AudioCodec
	if audioCodec == "" {
		audioCodec = "eac3"
	}
	channels := channelsFromString(info.Channels)
	audio := core.StreamInfo{
		Index: 1, Type: "audio", Codec: audioCodec,
		Channels: channels.count, ChannelLayout: channels.layout,
		SampleRate: 48000, Default: true,
	}
	if t != nil {
		audio.Language = t.Language
	}

	var dur time.Duration
	if t != nil {
		dur = t.Runtime
	}
	bitrate := 0
	switch {
	case rel.Size > 0 && dur > 0:
		bitrate = int(float64(rel.Size) * 8 / dur.Seconds() / 1000)
	case rel.Size > 0:
		bitrate = typicalVariantKbps(height, codec)
		if bitrate > 0 {
			dur = time.Duration(float64(rel.Size) * 8 / (float64(bitrate) * 1000) * float64(time.Second))
		}
	default:
		bitrate = typicalVariantKbps(height, codec)
	}
	size := rel.Size
	if size <= 0 && dur > 0 && bitrate > 0 {
		size = int64(float64(bitrate) * 1000 / 8 * dur.Seconds())
	}
	audio.BitrateKbps = audioKbpsGuess(audioCodec, channels.count)
	if bitrate > audio.BitrateKbps {
		video.BitrateKbps = bitrate - audio.BitrateKbps
	}

	container := info.Container
	if container == "" {
		container = "mkv"
	}
	return core.MediaInfo{
		Container:   container,
		Duration:    dur,
		Size:        size,
		BitrateKbps: bitrate,
		Streams:     []core.StreamInfo{video, audio},
		Estimated:   true,
	}
}

func variantWidth(h int) int {
	switch h {
	case 2160:
		return 3840
	case 1080:
		return 1920
	case 720:
		return 1280
	case 576:
		return 1024
	case 480:
		return 854
	default:
		return h * 16 / 9
	}
}

type channelInfo struct {
	count  int
	layout string
}

// channelsFromString parses "5.1"-style channel strings (a+b channels).
func channelsFromString(s string) channelInfo {
	switch s {
	case "7.1":
		return channelInfo{8, "7.1"}
	case "6.1":
		return channelInfo{7, "6.1"}
	case "5.1":
		return channelInfo{6, "5.1(side)"}
	case "2.1":
		return channelInfo{3, "2.1"}
	case "2.0", "1.0":
		return channelInfo{2, "stereo"}
	}
	if a, b, ok := strings.Cut(s, "."); ok {
		ai, err1 := strconv.Atoi(a)
		bi, err2 := strconv.Atoi(b)
		if err1 == nil && err2 == nil && ai > 0 {
			return channelInfo{ai + bi, s}
		}
	}
	return channelInfo{6, "5.1(side)"}
}

func audioKbpsGuess(codec string, channels int) int {
	switch codec {
	case "truehd", "dtshd", "flac":
		return 1500
	case "dts":
		return 768
	case "eac3", "ac3":
		if channels > 2 {
			return 640
		}
		return 192
	default: // aac, opus, mp3
		if channels > 2 {
			return 384
		}
		return 192
	}
}

// typicalVariantKbps mirrors playback's representative bitrates; kept local
// to avoid a resolver→playback dependency.
func typicalVariantKbps(height int, codec string) int {
	var kbps int
	switch height {
	case 2160:
		kbps = 20000
	case 1080:
		kbps = 8000
	case 720:
		kbps = 4000
	case 576:
		kbps = 2500
	default:
		kbps = 1500
	}
	if codec == "hevc" && height < 2160 || codec == "av1" {
		kbps = kbps * 6 / 10
	}
	return kbps
}
