// Package resolver implements core.Resolver: it turns a catalogue
// title plus client capabilities into a health-checked release.
package resolver

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// defaultProfile is used when config has no usable profiles at all.
func defaultProfile() core.QualityProfile {
	return core.QualityProfile{
		Name:             "default",
		MaxHeight:        1080,
		MinHeight:        720,
		PreferredSources: []core.Source{core.SourceWEBDL, core.SourceBluRay, core.SourceWEBRip, core.SourceHDTV},
		PreferredCodecs:  []string{"h264", "hevc"},
		PreferredAudio:   []string{"eac3", "ac3", "aac"},
		MinSizeMBPerMin:  5,
		MaxSizeMBPerMin:  150,
		Ranking:          core.DefaultRanking(nil),
	}
}

func clientDevice(caps core.ClientCaps) string {
	if caps.Device != "" {
		return caps.Device
	}
	return caps.DeviceName
}

// defaultCappedAndroidClient reports whether caps describes an Android
// mobile client whose registry playback ceiling is 1080p unless an owner
// quality rule lifts it. The match is the registry quirk
// android-mobile-ceiling, the same fact the capability engine uses.
func defaultCappedAndroidClient(caps core.ClientCaps) bool {
	return clientreg.HasQuirk(clientreg.Identity{Product: caps.Product, Platform: caps.Platform, Device: clientDevice(caps), Version: caps.Version}, "android-mobile-ceiling")
}

func isSurroundAudioCodec(c string) bool {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "eac3", "ec3", "e-ac3", "ac3", "dts", "dtshd", "dts-hd", "dtshd_ma", "dca", "dca-ma", "truehd", "mlp":
		return true
	}
	return false
}

func filterOutSurroundAudio(codecs []string) []string {
	var kept []string
	for _, c := range codecs {
		if !isSurroundAudioCodec(c) {
			kept = append(kept, c)
		}
	}
	return kept
}

// ProfileSource names the layer that picked a client's quality profile.
type ProfileSource string

const (
	ProfileFromRule    ProfileSource = "rule"    // the first matching quality rule
	ProfileFromRemote  ProfileSource = "remote"  // the Away profile, for a remote client no rule covers
	ProfileFromDefault ProfileSource = "default" // the At home (default) profile
	ProfileFromFirst   ProfileSource = "first"   // the first configured profile
	ProfileBuiltin     ProfileSource = "builtin" // no usable profile is configured
)

// profileChoice is which configured profile applies to a client, before
// any clamp, and why.
type profileChoice struct {
	profile core.QualityProfile
	source  ProfileSource
	rule    int // index into q.Rules when source is ProfileFromRule, else -1
}

// normalizeMedia keeps an unknown or empty media hint media-neutral.
func normalizeMedia(media string) string {
	switch strings.ToLower(strings.TrimSpace(media)) {
	case core.MediaMovie:
		return core.MediaMovie
	case core.MediaTV:
		return core.MediaTV
	}
	return core.MediaAny
}

// titleMedia classifies a title as a movie or an episode; a title with no
// usable type is unknown and returns "".
func titleMedia(t *core.Title) string {
	if t == nil {
		return core.MediaAny
	}
	switch t.Type {
	case core.TypeMovie:
		return core.MediaMovie
	case core.TypeEpisode:
		return core.MediaTV
	}
	switch t.Key.Kind() {
	case core.KindMovie:
		return core.MediaMovie
	case core.KindEpisode:
		return core.MediaTV
	}
	return core.MediaAny
}

// remoteProfileName is the Away profile for media: the media-specific
// choice when set, else the legacy media-neutral one.
func remoteProfileName(q config.Quality, media string) string {
	switch media {
	case core.MediaMovie:
		if q.RemoteMovie != "" {
			return q.RemoteMovie
		}
	case core.MediaTV:
		if q.RemoteTV != "" {
			return q.RemoteTV
		}
	}
	return q.Remote
}

// defaultProfileName is the At home profile for media: the media-specific
// choice when set, else the legacy media-neutral one.
func defaultProfileName(q config.Quality, media string) string {
	switch media {
	case core.MediaMovie:
		if q.DefaultMovie != "" {
			return q.DefaultMovie
		}
	case core.MediaTV:
		if q.DefaultTV != "" {
			return q.DefaultTV
		}
	}
	return q.Default
}

// profileApplies reports whether a profile may be chosen for media.
func profileApplies(p core.QualityProfile, media string) bool {
	return core.MediaApplies(p.MediaType, media)
}

// chooseProfile is the media-neutral form for callers with no title.
func chooseProfile(q config.Quality, caps core.ClientCaps, user *core.User) profileChoice {
	return chooseProfileForMedia(q, caps, user, core.MediaAny)
}

// chooseProfileForMedia is the single place that picks a client's profile.
//
// Selection order: the first rule whose non-empty matchers all match, whose
// media scope applies, and whose profile exists and applies; otherwise the
// media's remote profile for a remote client (when set and applicable);
// otherwise the media's default; otherwise the first applicable profile;
// otherwise the built-in default. A rule for a device or viewer is more
// specific than "away from home", so it wins there too. A movie-scoped
// profile is never chosen for a show, or the reverse.
func chooseProfileForMedia(q config.Quality, caps core.ClientCaps, user *core.User, media string) profileChoice {
	media = normalizeMedia(media)
	for i, rule := range q.Rules {
		if !core.MediaApplies(rule.MediaType, media) || !ruleMatches(rule, caps, user) {
			continue
		}
		if p, ok := core.FindProfile(q.Profiles, rule.Profile, media); ok {
			return profileChoice{profile: p, source: ProfileFromRule, rule: i}
		}
	}
	if caps.Remote {
		if p, ok := core.FindProfile(q.Profiles, remoteProfileName(q, media), media); ok {
			return profileChoice{profile: p, source: ProfileFromRemote, rule: -1}
		}
	}
	if p, ok := core.FindProfile(q.Profiles, defaultProfileName(q, media), media); ok {
		return profileChoice{profile: p, source: ProfileFromDefault, rule: -1}
	}
	for _, p := range q.Profiles {
		if profileApplies(p, media) {
			return profileChoice{profile: p, source: ProfileFromFirst, rule: -1}
		}
	}
	return profileChoice{profile: defaultProfile(), source: ProfileBuiltin, rule: -1}
}

// ruleEffect records what the matching quality rule did to the client's
// capabilities in applyClientRules.
type ruleEffect struct {
	choice profileChoice
	// ruleHeight is the rule's max height written over the client's ceiling.
	ruleHeight int
	// heightIgnored is set when the rule has a max height but the client
	// (or an owner App compatibility edit) fixed the height.
	heightIgnored bool
	// lifted is the height an Android mobile client's usual 1080p ceiling
	// was raised to because a rule picked a higher profile.
	lifted int
	// hdrLifted is set when that lift also turned HDR10 on.
	hdrLifted bool
}

func stripSurroundAudioCaps(caps core.ClientCaps) core.ClientCaps {
	if len(caps.AudioCodecs) == 0 {
		if defaults := clientreg.Playback(clientreg.Identity{Product: caps.Product, Platform: caps.Platform, Device: clientDevice(caps), Version: caps.Version}, nil); len(defaults.Audio) > 0 {
			caps.AudioCodecs = append([]string(nil), defaults.Audio...)
		} else {
			caps.AudioCodecs = []string{"aac", "mp3", "flac", "alac", "opus"}
		}
	}
	caps.AudioCodecs = filterOutSurroundAudio(caps.AudioCodecs)
	if len(caps.AudioCodecs) == 0 {
		caps.AudioCodecs = []string{"aac"}
	}
	if len(caps.TranscodeAudioCodecs) > 0 {
		caps.TranscodeAudioCodecs = filterOutSurroundAudio(caps.TranscodeAudioCodecs)
	}
	if caps.Profiles != nil {
		cp := *caps.Profiles
		cp.DirectPlay = slices.Clone(caps.Profiles.DirectPlay)
		for i := range cp.DirectPlay {
			base := cp.DirectPlay[i].AudioCodecs
			if len(base) == 0 {
				base = caps.AudioCodecs
			}
			filtered := filterOutSurroundAudio(base)
			if len(filtered) == 0 {
				filtered = []string{"aac"}
			}
			cp.DirectPlay[i].AudioCodecs = filtered
		}
		cp.Transcoding = slices.Clone(caps.Profiles.Transcoding)
		for i := range cp.Transcoding {
			filtered := filterOutSurroundAudio(cp.Transcoding[i].AudioCodecs)
			if len(filtered) == 0 {
				filtered = []string{"aac"}
			}
			cp.Transcoding[i].AudioCodecs = filtered
		}
		caps.Profiles = &cp
	}
	return caps
}

// ApplyClientRules returns caps with any matching user-defined client rule
// resolution override applied. When no rule matches (or when the client
// sent an explicit per-request resolution limit), the default 1080p ceiling
// for Android mobile is preserved. When the selected profile for the client
// allows 2160p (4K) and the request carries only the server's default 1080p
// remote bitrate cap (rather than an explicit client bitrate limit), the
// default remote bitrate cap is cleared so 4K releases are not demoted below
// 8 Mbps.
func ApplyClientRules(q config.Quality, caps core.ClientCaps, user *core.User) core.ClientCaps {
	return ApplyClientRulesForMedia(q, caps, user, core.MediaAny)
}

// ApplyClientRulesForMedia applies only shared policy until the title is known.
// Scoped rules must never irreversibly narrow another media's capabilities.
// A profile name is shared when every media has a profile of that name.
func ApplyClientRulesForMedia(q config.Quality, caps core.ClientCaps, user *core.User, media string) core.ClientCaps {
	if media == core.MediaAny {
		all := q.Profiles
		q.Profiles = slices.DeleteFunc(slices.Clone(q.Profiles), func(p core.QualityProfile) bool {
			return p.MediaType != core.MediaAny && !core.ProfileServesEveryMedia(all, p.Name)
		})
		q.Rules = slices.DeleteFunc(slices.Clone(q.Rules), func(r config.QualityRule) bool { return r.MediaType != core.MediaAny })
	}
	caps, _ = applyClientRules(q, caps, user, media)
	return caps
}

// ApplyClientRulesForTitle applies the rules that match a specific title's
// media, so a movie rule cannot change a show's caps or the reverse.
func ApplyClientRulesForTitle(q config.Quality, caps core.ClientCaps, user *core.User, t *core.Title) core.ClientCaps {
	caps, _ = applyClientRules(q, caps, user, titleMedia(t))
	return caps
}

func applyClientRules(q config.Quality, caps core.ClientCaps, user *core.User, media string) (core.ClientCaps, ruleEffect) {
	if len(caps.VideoCodecs) == 0 && caps.Protocol != "plex" && !strings.Contains(strings.ToLower(caps.Product), "plex") {
		if defaults := clientreg.Playback(clientreg.Identity{Product: caps.Product, Platform: caps.Platform, Device: clientDevice(caps), Version: caps.Version}, nil); defaults.ID != "" && defaults.ID != "generic" && len(defaults.Video) > 0 {
			caps.VideoCodecs = append([]string(nil), defaults.Video...)
		}
	}
	// Rule matchers read only protocol, product, platform and viewer, none
	// of which this function changes, so the choice holds for the result.
	eff := ruleEffect{choice: chooseProfileForMedia(q, caps, user, media)}
	if caps.Download {
		return caps, eff
	}
	if eff.choice.source == ProfileFromRule {
		rule, prof := q.Rules[eff.choice.rule], eff.choice.profile
		targetHeight := rule.MaxHeight
		if targetHeight <= 0 {
			if prof.MaxHeight > 0 {
				targetHeight = prof.MaxHeight
			} else {
				targetHeight = 2160
			}
		}
		if !caps.ExplicitMaxHeight {
			if rule.MaxHeight > 0 {
				caps.MaxHeight = rule.MaxHeight
				eff.ruleHeight = rule.MaxHeight
			} else if defaultCappedAndroidClient(caps) && caps.MaxHeight < targetHeight {
				caps.MaxHeight = targetHeight
				eff.lifted = targetHeight
			}
		} else if rule.MaxHeight > 0 {
			eff.heightIgnored = true
		}
		if caps.MaxHeight >= 2160 && prof.AllowHDR && !caps.ExplicitNoHDR && defaultCappedAndroidClient(caps) {
			eff.hdrLifted = !caps.HDR10
			caps.HDR10 = true
		}
		if rule.ConvertSurroundAudio {
			caps = stripSurroundAudioCaps(caps)
		}
	}
	selected := eff.choice.profile
	if eff.choice.source != ProfileBuiltin && caps.DefaultRemoteBitrate && (selected.MaxHeight == 0 || selected.MaxHeight >= 2160) && (caps.MaxHeight == 0 || caps.MaxHeight >= 2160) {
		caps.MaxBitrateKbps = 0
		caps.DefaultRemoteBitrate = false
	}
	return caps, eff
}

// SelectProfile picks the quality profile for a client and clamps it
// to what the client can direct-play. Playback uses it for
// pre-resolution estimates; the resolver uses it for ranking.
//
// Selection order is chooseProfile's. Clamping: MaxHeight is capped at
// caps.MaxHeight, HDR/DV are only allowed when the client supports them,
// and preferred codecs the client cannot decode are dropped.
func SelectProfile(q config.Quality, caps core.ClientCaps, user *core.User) core.QualityProfile {
	return SelectProfileForMedia(q, caps, user, core.MediaAny)
}

// SelectProfileForMedia is SelectProfile with a movie or show context, so
// media-scoped profiles and rules apply. An empty media is media-neutral.
func SelectProfileForMedia(q config.Quality, caps core.ClientCaps, user *core.User, media string) core.QualityProfile {
	return decideProfile(q, caps, user, media).profile
}

// profileDecision is SelectProfile's answer with what decided it.
type profileDecision struct {
	profile core.QualityProfile // selected, then clamped to the client
	caps    core.ClientCaps     // the client after quality rules
	effect  ruleEffect
}

func decideProfile(q config.Quality, caps core.ClientCaps, user *core.User, media string) profileDecision {
	if caps.Download && caps.DownloadSourceMaxHeight > caps.MaxHeight {
		caps.MaxHeight = caps.DownloadSourceMaxHeight
	}
	caps, eff := applyClientRules(q, caps, user, media)
	prof := eff.choice.profile
	// Deep-copy slices so callers cannot mutate config.
	prof.PreferredSources = append([]core.Source(nil), prof.PreferredSources...)
	prof.PreferredCodecs = append([]string(nil), prof.PreferredCodecs...)
	prof.PreferredAudio = append([]string(nil), prof.PreferredAudio...)
	prof.PreferredLanguages = append([]string(nil), prof.PreferredLanguages...)
	prof.AllowedQualities = append([]string(nil), prof.AllowedQualities...)
	prof.QualityOrder = cloneQualityOrder(prof.QualityOrder)
	prof.Ranking = append([]string(nil), prof.Ranking...)
	if prof.RankingConfig != nil {
		cloned := make(map[string]json.RawMessage, len(prof.RankingConfig))
		for k, v := range prof.RankingConfig {
			cloned[k] = append(json.RawMessage(nil), v...)
		}
		prof.RankingConfig = cloned
	}
	if prof.CustomFormatScores != nil {
		copyScores := map[string]int{}
		for k, v := range prof.CustomFormatScores {
			copyScores[k] = v
		}
		prof.CustomFormatScores = copyScores
	}
	if len(prof.ReleaseRules) > 0 {
		rules := make([]core.ReleaseRule, len(prof.ReleaseRules))
		for i, r := range prof.ReleaseRules {
			rules[i] = r
			rules[i].Conditions = append([]core.ReleaseCondition(nil), r.Conditions...)
			rules[i].Unless = append([]core.ReleaseCondition(nil), r.Unless...)
			rules[i].Fallback = append([]core.ReleaseCondition(nil), r.Fallback...)
		}
		prof.ReleaseRules = rules
	}

	// Key of the configured definition, taken from this pristine copy
	// before the configured-height snapshot, download reshaping, and
	// the client-cap clamp below, so the same configured profile keys
	// identically for every client.
	prof.SelectionKey = prof.ComputeSelectionKey()

	// Snapshot the owner's configured ceiling before the download
	// reshape below (and the client-cap clamp after it) lowers the
	// effective MaxHeight: "configured" is what the owner set, even
	// when the reshape is what ends up limiting the release.
	prof = snapshotConfiguredMaxHeight(prof)

	if caps.Download {
		prof = downloadProfile(prof, caps)
	}

	// Clamp to client capabilities.
	if caps.MaxHeight > 0 && (prof.MaxHeight == 0 || prof.MaxHeight > caps.MaxHeight) {
		prof.MaxHeight = caps.MaxHeight
	}
	if prof.MinHeight > prof.MaxHeight && prof.MaxHeight > 0 {
		prof.MinHeight = prof.MaxHeight
	}
	prof.AllowHDR = prof.AllowHDR && caps.HDR10
	prof.AllowDolbyVision = prof.AllowDolbyVision && caps.DolbyVision
	if len(caps.VideoCodecs) > 0 {
		kept := prof.PreferredCodecs[:0:0]
		for _, c := range prof.PreferredCodecs {
			if caps.SupportsVideo(c) {
				kept = append(kept, c)
			}
		}
		prof.PreferredCodecs = kept
	}
	if eff.choice.source == ProfileFromRule && q.Rules[eff.choice.rule].ConvertSurroundAudio && len(caps.AudioCodecs) > 0 {
		kept := prof.PreferredAudio[:0:0]
		for _, a := range prof.PreferredAudio {
			if caps.SupportsAudio(a) {
				kept = append(kept, a)
			}
		}
		prof.PreferredAudio = kept
	}
	return profileDecision{profile: prof, caps: caps, effect: eff}
}

// downloadProfile reshapes the selected profile for an offline download
// (caps.Download): a compact, mobile-native file that stream-copies into
// an MP4. No 4K/HDR/Dolby Vision (a copy cannot tone-map, and phones do
// not need it), WEB-DL first, H.264 — or HEVC when the device decodes
// it — and MP4-friendly AAC/AC3/EAC3 audio only (no TrueHD/DTS-HD,
// which neither copies into a broadly playable MP4 nor helps a phone).
// The size cap (~10 Mbps average) keeps a two-hour film near 9 GB worst
// case and typical 1080p WEB-DLs well inside it. caps.MaxHeight and
// caps.MaxBitrateKbps from the client's videoQuality/videoResolution
// request still clamp on top of this in SelectProfile.
func downloadProfile(prof core.QualityProfile, caps core.ClientCaps) core.QualityProfile {
	prof.Name += "+download"
	ceiling := 1080
	if caps.DownloadSourceMaxHeight > ceiling {
		ceiling = caps.DownloadSourceMaxHeight
	}
	if prof.MaxHeight == 0 || prof.MaxHeight > ceiling {
		prof.MaxHeight = ceiling
	}
	if prof.MinHeight > prof.MaxHeight {
		prof.MinHeight = prof.MaxHeight
	}
	prof.AllowHDR = false
	prof.AllowDolbyVision = false
	prof.PreferredSources = []core.Source{core.SourceWEBDL, core.SourceWEBRip, core.SourceBluRay, core.SourceHDTV}
	if len(caps.VideoCodecs) == 0 || caps.SupportsVideo("hevc") {
		prof.PreferredCodecs = []string{"h264", "hevc"}
	} else {
		prof.PreferredCodecs = []string{"h264"}
	}
	prof.PreferredAudio = []string{"aac", "eac3", "ac3"}
	// A source for conversion can be larger than the compact copy target;
	// keep the owner's size ceilings instead of adding the copy-only cap.
	if caps.DownloadSourceMaxHeight == 0 {
		if prof.MaxSizeMBPerMin == 0 || prof.MaxSizeMBPerMin > 80 {
			prof.MaxSizeMBPerMin = 80
		}
		if prof.EpisodeMaxSizeMBPerMin > 80 {
			prof.EpisodeMaxSizeMBPerMin = 80
		}
	}
	prof.PreferLargest = false
	return prof
}

// configuredUnlimited marks a profile whose owner set no max-height
// ceiling ("Any", MaxHeight 0 in config). It keeps "no ceiling" apart
// from ConfiguredMaxHeight 0 ("field not set") on hand-built profiles.
const configuredUnlimited = -1

// snapshotConfiguredMaxHeight records the owner's ceiling on p before
// any reshape or clamp lowers MaxHeight.
func snapshotConfiguredMaxHeight(p core.QualityProfile) core.QualityProfile {
	if p.MaxHeight == 0 {
		p.ConfiguredMaxHeight = configuredUnlimited
	} else {
		p.ConfiguredMaxHeight = p.MaxHeight
	}
	return p
}

// configuredMaxHeight is the profile's own ceiling before any download
// reshape or client clamp: configuredUnlimited when the owner set
// none, otherwise a positive height. Profiles built outside
// SelectProfile (ConfiguredMaxHeight unset) report their MaxHeight.
func configuredMaxHeight(p core.QualityProfile) int {
	if p.ConfiguredMaxHeight != 0 {
		return p.ConfiguredMaxHeight
	}
	return p.MaxHeight
}

// traceConfiguredMaxHeight is the configured ceiling as recorded on a
// trace or history record, with presence: a profile from SelectProfile
// reports its configured ceiling (a pointer to 0 when the owner set
// none — explicitly unlimited), while a profile built outside
// SelectProfile (ConfiguredMaxHeight unset) reports nil, unknown,
// rather than falling back to its effective MaxHeight.
func traceConfiguredMaxHeight(p core.QualityProfile) *int {
	switch {
	case p.ConfiguredMaxHeight == configuredUnlimited:
		v := 0
		return &v
	case p.ConfiguredMaxHeight > 0:
		v := p.ConfiguredMaxHeight
		return &v
	default:
		return nil
	}
}

// productMatches is a case-insensitive substring match on X-Plex-Product.
// "Fire TV" is matched by device instead: the app there reports itself as
// "Plex for Android (TV)", so the console's Fire TV preset never matched.
func productMatches(want string, caps core.ClientCaps) bool {
	w := strings.ToLower(strings.TrimSpace(want))
	if strings.Contains(strings.ToLower(caps.Product), w) {
		return true
	}
	return (w == "fire tv" || w == "firetv") && core.IsFireTV(caps.Product, caps.Device)
}

func ruleMatches(rule config.QualityRule, caps core.ClientCaps, user *core.User) bool {
	if rule.Protocol != "" && !strings.EqualFold(string(rule.Protocol), string(caps.Protocol)) {
		return false
	}
	if rule.Product != "" && !productMatches(rule.Product, caps) {
		return false
	}
	if rule.Platform != "" && !strings.Contains(strings.ToLower(caps.Platform), strings.ToLower(rule.Platform)) {
		return false
	}
	if rule.UserID != 0 {
		if user == nil || user.ID != rule.UserID {
			return false
		}
	}
	// A rule with no matchers at all would match everything; require
	// at least one matcher to avoid surprises from empty config rows.
	return rule.Protocol != "" || rule.Product != "" || rule.Platform != "" || rule.UserID != 0
}
