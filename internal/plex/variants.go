package plex

// "Play Version" support (roadmap item #8): official Plex apps render a
// native version picker whenever a <Video> item has several <Media>
// children. This file renders one Media per distinct candidate release on
// detail pages, gives non-Auto variants stable integer Media/Part ids,
// and maps a viewer's selection (mediaIndex on decision/start requests,
// or a variant Part id on direct-play requests) back to the chosen
// release for core.VariantPlayback.PrepareVariant.
//
// PMS shapes (python-plexapi plexapi/media.py, plexapi/video.py):
//   - Media carries `title` (the version name Plex apps show for named
//     versions; optimized versions also carry `target`/`proxyType`, which
//     Ether does not claim) — Ether sets `title` to the variant label.
//   - Movie editions use `editionTitle` on the <Video> item — Ether sets
//     it to the Auto variant's cut when the default pick is a cut.
//
// Ids: rating keys already sit near 10^15 with kind digits 1-9 at 10^15.
// Variant ids are issued from 9*10^15 and stay below 2^53 for JavaScript
// clients (the block older notes called 8*10^15). That range overlaps
// KindLocal rating keys. The registry is the authority for a variant;
// a subtitle choice whose part id is in the block but not in the registry
// follows the stream's title instead of that guessed key. The id is a
// hash of (ratingKey, release ID) with
// linear collision probing; mappings are persisted in the variants_ids
// table so old part keys keep working across restarts (without a DB they
// are recomputed deterministically, the same way a Plex stream id is).
// The Auto variant keeps Media.ID == Part.ID == ratingKey, exactly as
// before variants existed.

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("variants", 1, `
CREATE TABLE variants_ids (
  media_id INTEGER PRIMARY KEY,
  rating_key INTEGER NOT NULL,
  release_id TEXT NOT NULL
);
CREATE UNIQUE INDEX variants_ids_pair ON variants_ids(rating_key, release_id);`)
}

const (
	variantIDBase  = 9_000_000_000_000_000
	variantIDSpace = 7_199_254_740_990 // ids stay below 2^53

	maxVariantIDs       = 200_000
	renderedVariantsTTL = 30 * time.Minute
	maxRenderedTitles   = 10_000
	variantRenderBudget = 3 * time.Second // incl. the resolver's in-flight wait
)

// ---------------------------------------------------------------------------
// Variant Media/Part ids

type variantRef struct {
	key       core.RatingKey
	releaseID string
}

type variantIDRegistry struct {
	mu     sync.Mutex
	db     *sql.DB
	byID   map[int64]variantRef
	byPair map[variantRef]int64
	loaded bool
}

func newVariantIDRegistry(db *sql.DB) *variantIDRegistry {
	return &variantIDRegistry{db: db, byID: map[int64]variantRef{}, byPair: map[variantRef]int64{}}
}

// loadLocked reads persisted mappings once; the caller holds mu.
func (r *variantIDRegistry) loadLocked() {
	if r.loaded {
		return
	}
	r.loaded = true
	if r.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, `SELECT media_id, rating_key, release_id FROM variants_ids`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, rk int64
		var rel string
		if rows.Scan(&id, &rk, &rel) == nil {
			ref := variantRef{key: core.RatingKey(rk), releaseID: rel}
			r.byID[id] = ref
			r.byPair[ref] = id
		}
	}
	if err := rows.Err(); err != nil {
		slog.Default().Warn("plex: variant ids not fully loaded", "err", err)
	}
}

func variantHash(key core.RatingKey, releaseID string) int64 {
	h := fnv.New64a()
	h.Write([]byte(strconv.FormatInt(int64(key), 10) + "/" + releaseID))
	return variantIDBase + int64(h.Sum64()%variantIDSpace) + 1
}

// idFor returns the stable Media/Part id of a non-Auto variant.
func (r *variantIDRegistry) idFor(key core.RatingKey, releaseID string) int64 {
	ref := variantRef{key: key, releaseID: releaseID}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadLocked()
	if id, ok := r.byPair[ref]; ok {
		return id
	}
	if len(r.byID) >= maxVariantIDs && r.db == nil {
		r.byID = map[int64]variantRef{}
		r.byPair = map[variantRef]int64{}
	}
	id := variantHash(key, releaseID)
	for {
		existing, ok := r.byID[id]
		if !ok || existing == ref {
			break
		}
		id++ // collision: probe (the space is ~10^15 wide, so this terminates)
		if id >= variantIDBase+variantIDSpace {
			id = variantIDBase + 1
		}
	}
	r.byID[id] = ref
	r.byPair[ref] = id
	if r.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = r.db.ExecContext(ctx,
			`INSERT INTO variants_ids(media_id, rating_key, release_id) VALUES (?,?,?) ON CONFLICT DO NOTHING`,
			id, int64(key), releaseID)
		cancel()
	}
	return id
}

func (r *variantIDRegistry) lookup(id int64) (variantRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadLocked()
	ref, ok := r.byID[id]
	return ref, ok
}

// inVariantIDSpace reports ids that Play Version may have issued. The live
// allocator starts at 9*10^15 and stops below 2^53; the 8*10^15 block is
// the same namespace from before that move. Both overlap real rating-key
// kinds, so callers must not treat a miss as a title.
func inVariantIDSpace(id int64) bool {
	return id >= 8_000_000_000_000_000 && id < 1<<53
}

// partTitle maps a Part id back to its title: the rating key itself for
// the Auto part, or a registry id for a variant part (with its release).
// releaseID is non-empty only when the registry resolved a variant.
func (s *Server) partTitle(partID int64) (key core.RatingKey, releaseID string, ok bool) {
	if s.variantIDs != nil {
		if ref, found := s.variantIDs.lookup(partID); found {
			return ref.key, ref.releaseID, true
		}
	}
	// Local-file keys share this numeric range, so a miss stays a playable
	// key when it is one. Subtitle selection does not trust that guess:
	// handlePartSelect keeps the stream's own title when the part id is in
	// the variant block and the registry did not claim it.
	k := core.RatingKey(partID)
	if k.Valid() && isPlayableKey(k) {
		return k, "", true
	}
	return 0, "", false
}

// ---------------------------------------------------------------------------
// Rendered variant lists (for mediaIndex resolution)

type renderedVariantKey struct {
	userID   int64
	clientID string
	key      core.RatingKey
}

type renderedVariantSet struct {
	releaseIDs []string // rendered order, Auto first
	at         time.Time
}

// renderedVariants remembers, per viewer+client+title, the variant order
// last rendered on a detail page. Clients select a version by position
// (mediaIndex) without naming the release, so playback must resolve the
// position against what that viewer actually saw — variant order depends
// on the client's capabilities.
type renderedVariants struct {
	mu sync.Mutex
	m  map[renderedVariantKey]renderedVariantSet
}

func newRenderedVariants() *renderedVariants {
	return &renderedVariants{m: map[renderedVariantKey]renderedVariantSet{}}
}

func (r *renderedVariants) put(k renderedVariantKey, releaseIDs []string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) >= maxRenderedTitles {
		r.m = map[renderedVariantKey]renderedVariantSet{}
	}
	r.m[k] = renderedVariantSet{releaseIDs: append([]string(nil), releaseIDs...), at: now}
}

func (r *renderedVariants) get(k renderedVariantKey, now time.Time) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[k]
	if !ok || now.Sub(e.at) > renderedVariantsTTL {
		return nil, false
	}
	return append([]string(nil), e.releaseIDs...), true
}

func (r *renderedVariants) expire(now time.Time, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.m {
		if now.Sub(e.at) > ttl {
			delete(r.m, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Rendering

// mediaForVariants renders one Media per release variant for a detail
// page. It returns (nil, "") to fall back to the single virtual Media:
// no variant resolver wired, nothing cached yet, or only Auto exists.
// The second value is the item's editionTitle, or "".
func (s *Server) mediaForVariants(ctx context.Context, pr *Request, t *core.Title, stamp int64, probed core.MediaInfo, probedRelease string) ([]model.Media, string) {
	if s.d.Variants == nil || pr.User == nil {
		return nil, ""
	}
	caps := s.clientCaps(pr)
	vctx, cancel := context.WithTimeout(ctx, variantRenderBudget)
	defer cancel()
	vs, err := s.d.Variants.Variants(vctx, core.ResolveRequest{Title: t, Client: caps, User: pr.User, PlayedRelease: probedRelease}, core.MaxVariants)
	if err != nil {
		s.log.Debug("variants unavailable", "key", t.Key, "err", err)
		return nil, ""
	}
	if len(vs) < 2 {
		return nil, ""
	}
	ids := make([]string, len(vs))
	for i, v := range vs {
		ids[i] = v.Release.ID
	}
	s.renderedVariants.put(renderedVariantKey{userID: pr.User.ID, clientID: pr.ClientID, key: t.Key}, ids, s.now())

	alignPlayedAutoVariant(vs, probedRelease)
	probedReady := !probed.Estimated && len(probed.Streams) > 0 && probed.Video() != nil
	matchedNonAuto := false
	if probedRelease != "" && probedReady && probedRelease != vs[0].Release.ID {
		for _, v := range vs {
			if !v.Auto && v.Release.ID == probedRelease {
				matchedNonAuto = true
				break
			}
		}
	}
	autoDuplicatesRow := false
	for _, v := range vs[1:] {
		if sameVariantRelease(v.Release, vs[0].Release) {
			autoDuplicatesRow = true
			break
		}
	}
	out := make([]model.Media, 0, len(vs))
	for _, v := range vs {
		// Until this viewer has a resumed release, only Auto carries the
		// stream list (the completed detail warm-up probe when available,
		// otherwise the estimate). A resumed probe is drawn on the matching
		// version only, never on a different explicit pick, and that version
		// is selected; when the resumed release is not one of the explicit
		// non-Auto picks (e.g. Auto resolved past an uncached top candidate),
		// it belongs to Auto.
		played := probedRelease != "" && probedReady && ((v.Auto && !matchedNonAuto) || (!v.Auto && matchedNonAuto && v.Release.ID == probedRelease))
		warmAuto := probedRelease == "" && probedReady && v.Auto
		variant := v
		if played || warmAuto {
			keepReleaseBitrate := !probeDisagrees(v.Info, probed) && v.Info.BitrateKbps > 0 && (probedRelease != "" || autoDuplicatesRow || probed.BitrateKbps <= 0)
			variant.Info = mergeProbedVariantInfo(v.Info, probed, keepReleaseBitrate)
		} else if !v.Auto && sameVariantRelease(v.Release, vs[0].Release) && probedReady && !matchedNonAuto && !probeDisagrees(v.Info, probed) {
			variant.Info = mergeProbedVariantInfo(v.Info, probed, v.Info.BitrateKbps > 0)
		}
		m := buildMediaVariant(t, variant, stamp, s.variantIDs)
		if s.d.Trickplay != nil && s.d.Trickplay.TrickplayAvailable(v.Release.ID) {
			m.Part[0].Indexes = "sd"
		}
		if v.Auto || played {
			info := s.withCachedSidecars(ctx, pr.User, t, variant.Info)
			stored := s.selections.get(pr.User.ID, int64(t.Key))
			if stored.subtitleSet {
				info = infoWithSelectedSidecar(info, stored.subtitleStream)
			}
			sel := resolveSelection(stored, info)
			sel = s.preferredSelection(ctx, pr.User, t.Key, info, t.Language, sel)
			m.Part[0].Stream = s.buildStreams(t.Key, info, sel, pr.User)
		}
		if played {
			m.Selected = model.Ptr(true)
			m.Part[0].Selected = model.Ptr(true)
		}
		// Auto's title is the release-name estimate. When the file that
		// actually played or warmed is a different resolution or codec,
		// keep the parsed release metadata (source, group) and replace only
		// what the probe proves wrong.
		if v.Auto && (played || warmAuto) && probeDisagrees(v.Info, probed) {
			m.Extra = []model.Attr{model.A("title", probedAutoTitle(v.Release, v.Info, probed))}
		}
		out = append(out, m)
	}
	// A recorded PMS movie has one Media and does not set Media@selected
	// (testdata/conform/plex/web movie metadata). A recorded Plex Web
	// decision (hasMDE=1) is the one Media the client already chose, marked
	// selected="1". Search and library rows show the first Media, which is
	// Auto, so a 720p profile reads as 720p there. Plex Web then walks the
	// Play Version list itself and requests that part. With several versions
	// and nothing resumed, mark Auto so a client that honors selected starts
	// there. Clients that ignore it and take the tallest Media stay inside
	// the profile because taller versions are not listed unless the owner
	// opts in.
	if !anyMediaSelected(out) && len(out) > 0 {
		out[0].Selected = model.Ptr(true)
		if len(out[0].Part) > 0 {
			out[0].Part[0].Selected = model.Ptr(true)
		}
	}
	edition := ""
	if t.Type == core.TypeMovie {
		edition = vs[0].Edition
	}
	return out, edition
}

// buildMediaVariant renders one variant's Media/Part. Auto keeps the
// historical ids (ratingKey); other variants get their registry id and
// the variant label as the Media title Plex apps show in "Play Version".
func anyMediaSelected(media []model.Media) bool {
	for i := range media {
		if media[i].Selected != nil && *media[i].Selected {
			return true
		}
	}
	return false
}

func buildMediaVariant(t *core.Title, v core.ReleaseVariant, stamp int64, registry *variantIDRegistry) model.Media {
	m := buildMedia(t, v.Info, stamp)
	var id int64
	if v.Auto || registry == nil {
		id = int64(t.Key)
	} else {
		id = registry.idFor(t.Key, v.Release.ID)
	}
	if id != int64(t.Key) {
		ext := containerExt(plexContainer(v.Info.Container))
		m.ID = id
		m.Part[0].ID = id
		m.Part[0].Key = fmt.Sprintf("/library/parts/%d/%d/file.%s", id, stamp, ext)
	}
	if v.Label != "" {
		m.Extra = []model.Attr{model.A("title", v.Label)}
	}
	return m
}

// applyEditionTitle sets the item's editionTitle (PMS's edition label for
// movies) when the default version is a distinct cut.
func applyEditionTitle(m *model.Metadata, edition string) {
	if edition == "" {
		return
	}
	m.Extra = append(m.Extra, model.A("editionTitle", edition))
}

// ---------------------------------------------------------------------------
// Selection → playback

// resolveVariantSelection maps a play request's mediaIndex back to the
// release the viewer picked, using the list rendered for them; when that
// memory is gone (server restart, TTL) it recomputes the list, which is
// deterministic for the same cached search results and client. It
// returns "" for Auto (or when the index cannot be resolved).
// partIDFromPath extracts the numeric Part id from a
// "/library/parts/{id}/..." path, as sent by clients that address the
// chosen version's part key in decision/start requests.
func partIDFromPath(p string) (int64, bool) {
	i := strings.Index(p, "/library/parts/")
	if i < 0 {
		return 0, false
	}
	p = p[i+len("/library/parts/"):]
	p, _, _ = strings.Cut(p, "/")
	p, _, _ = strings.Cut(p, "?")
	id, err := strconv.ParseInt(p, 10, 64)
	return id, err == nil
}

func (s *Server) resolveVariantSelection(ctx context.Context, pr *Request, p *playRequest) string {
	if p.partReleaseID != "" {
		return p.partReleaseID
	}
	if p.mediaIndex <= 0 || pr.User == nil {
		return ""
	}
	k := renderedVariantKey{userID: pr.User.ID, clientID: pr.ClientID, key: p.key}
	if ids, ok := s.renderedVariants.get(k, s.now()); ok {
		if p.mediaIndex < len(ids) {
			return ids[p.mediaIndex]
		}
		s.log.Info("variant mediaIndex out of range; playing auto version",
			"key", p.key, "mediaIndex", p.mediaIndex, "rendered", len(ids))
		return ""
	}
	if s.d.Variants == nil || s.d.Catalog == nil {
		return ""
	}
	t, err := s.d.Catalog.Get(ctx, p.key)
	if err != nil {
		return ""
	}
	vctx, cancel := context.WithTimeout(ctx, variantRenderBudget)
	defer cancel()
	vs, err := s.d.Variants.Variants(vctx, core.ResolveRequest{Title: t, Client: p.caps, User: pr.User}, core.MaxVariants)
	if err != nil || p.mediaIndex >= len(vs) {
		return ""
	}
	return vs[p.mediaIndex].Release.ID
}

// alignPlayedAutoVariant updates Auto in place when the played release is a
// same-tier alternative in vs[1:] and the VariantResolver did not already
// align vs[0] (e.g. a static test stub).
func alignPlayedAutoVariant(vs []core.ReleaseVariant, playedRelease string) {
	if playedRelease == "" || len(vs) < 2 || !vs[0].Auto || vs[0].Release.ID == playedRelease {
		return
	}
	autoClass := streamResolutionClass(vs[0].Info)
	for _, v := range vs[1:] {
		if v.Release.ID != playedRelease {
			continue
		}
		if v.Edition != vs[0].Edition || streamResolutionClass(v.Info) != autoClass || hasDolbyVision(v.Info) != hasDolbyVision(vs[0].Info) || hasHDR10(v.Info) != hasHDR10(vs[0].Info) {
			return
		}
		lbl := v.Label
		if v.Release.Name != "" || v.Release.Info.Height > 0 || v.Release.Info.Source != "" {
			lbl = plexAutoLabel(v.Release.Info)
		} else if lbl != "" && !strings.HasPrefix(lbl, "Auto") {
			lbl = "Auto · " + lbl
		}
		vs[0] = core.ReleaseVariant{
			ID:      v.Release.ID,
			Label:   lbl,
			Edition: v.Edition,
			Auto:    true,
			Release: v.Release,
			Info:    v.Info,
		}
		return
	}
}

func sameVariantRelease(a, b core.Release) bool {
	if a.ID != "" && a.ID == b.ID {
		return true
	}
	na := strings.TrimSpace(a.Name)
	nb := strings.TrimSpace(b.Name)
	return na != "" && strings.EqualFold(na, nb)
}

func mergeProbedVariantInfo(claimed, probed core.MediaInfo, keepReleaseBitrate bool) core.MediaInfo {
	out := probed
	if keepReleaseBitrate && claimed.BitrateKbps > 0 {
		out.BitrateKbps = claimed.BitrateKbps
	}
	if out.Size <= 0 && claimed.Size > 0 {
		out.Size = claimed.Size
	}
	if out.Duration <= 0 && claimed.Duration > 0 {
		out.Duration = claimed.Duration
	}
	return out
}

func hasDolbyVision(info core.MediaInfo) bool {
	v := info.Video()
	return v != nil && v.DolbyVision
}

func hasHDR10(info core.MediaInfo) bool {
	v := info.Video()
	return v != nil && (v.ColorTransfer == "smpte2084" || v.ColorTransfer == "arib-std-b67")
}

// probeDisagrees reports that the file which played is not the picture or
// audio the release name (or its stand-in) described.
func probeDisagrees(claimed, probed core.MediaInfo) bool {
	if probed.Video() == nil {
		return false
	}
	if streamResolutionClass(claimed) != streamResolutionClass(probed) {
		return true
	}
	return audioCodecFamily(primaryAudioCodec(claimed)) != audioCodecFamily(primaryAudioCodec(probed))
}

func streamResolutionClass(info core.MediaInfo) int {
	if v := info.Video(); v != nil {
		return resolutionClass(v.Width, v.Height)
	}
	return 0
}

func resolutionClass(w, h int) int {
	switch {
	case w >= 3200 || h >= 1800:
		return 2160
	case w >= 1700 || h >= 1000:
		return 1080
	case w >= 1200 || h >= 700:
		return 720
	case w > 0 || h > 0:
		return 480
	default:
		return 0
	}
}

func audioCodecFamily(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "e-ac3", "ec3", "eac3":
		return "eac3"
	case "ac-3", "ac3":
		return "ac3"
	case "dca", "dts", "dtshd", "dts-hd", "dca-ma":
		return "dts"
	case "truehd", "mlp":
		return "truehd"
	case "mp4a", "aac":
		return "aac"
	default:
		return strings.ToLower(strings.TrimSpace(c))
	}
}

func primaryAudioCodec(info core.MediaInfo) string {
	fallback := ""
	for _, st := range info.Streams {
		if st.Type != "audio" || st.Codec == "" {
			continue
		}
		if st.Default || fallback == "" {
			fallback = strings.ToLower(st.Codec)
			if st.Default {
				return fallback
			}
		}
	}
	return fallback
}

// probedAutoTitle keeps the release-name metadata (source, group, audio
// details that still match) and replaces only what the probe proves wrong.
func probedAutoTitle(rel core.Release, claimed, probed core.MediaInfo) string {
	info := rel.Info
	probedClass := streamResolutionClass(probed)
	if streamResolutionClass(claimed) != probedClass || info.Height <= 0 {
		switch probedClass {
		case 2160, 1080, 720:
			info.Height = probedClass
		default:
			info.Height = 0
		}
		if v := probed.Video(); v != nil {
			info.DolbyVision = v.DolbyVision
			info.HDR10 = v.ColorTransfer == "smpte2084" || v.ColorTransfer == "arib-std-b67"
		}
	}
	probedAudio := primaryAudioCodec(probed)
	if audioCodecFamily(primaryAudioCodec(claimed)) != audioCodecFamily(probedAudio) || info.AudioCodec == "" {
		info.AudioCodec = audioCodecFamily(probedAudio)
		info.Atmos = false
		info.Channels = ""
	}
	return plexAutoLabel(info)
}

func plexAutoLabel(info core.ReleaseInfo) string {
	parts := []string{"Auto", plexQualityPhrase(info)}
	if a := plexAudioPhrase(info); a != "" {
		parts = append(parts, a)
	}
	if g := strings.TrimSpace(info.Group); g != "" {
		parts = append(parts, g)
	}
	return strings.Join(parts, " · ")
}

func plexQualityPhrase(info core.ReleaseInfo) string {
	res := "SD"
	switch {
	case info.Height >= 1800:
		res = "4K"
	case info.Height >= 1000:
		res = "1080p"
	case info.Height >= 700:
		res = "720p"
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
	switch info.Source {
	case core.SourceRemux:
		parts = append(parts, "Remux")
	case core.SourceWEBDL:
		parts = append(parts, "WEB-DL")
	case core.SourceBluRay:
		parts = append(parts, "Blu-ray")
	case core.SourceWEBRip:
		parts = append(parts, "WEBRip")
	case core.SourceHDTV:
		parts = append(parts, "HDTV")
	case core.SourceDVD:
		parts = append(parts, "DVD")
	case core.SourceScreener:
		parts = append(parts, "Screener")
	case core.SourceCam:
		parts = append(parts, "CAM")
	}
	return strings.Join(parts, " ")
}

func plexAudioPhrase(info core.ReleaseInfo) string {
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

// preparePlayback prepares a play session, honouring a "Play Version"
// pick when the playback manager supports it.
func (s *Server) preparePlayback(ctx context.Context, pr *Request, p *playRequest) (*core.PlaySession, error) {
	releaseID := s.resolveVariantSelection(ctx, pr, p)
	return s.prepareForRelease(ctx, pr.User, p.key, p.caps, releaseID)
}

// prepareForRelease prepares a session on a specific release ("" = Auto).
// When the playback manager cannot target a release, it logs and plays
// the normal choice instead of failing.
func (s *Server) prepareForRelease(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, releaseID string) (*core.PlaySession, error) {
	if releaseID != "" {
		if vp, ok := s.d.Playback.(core.VariantPlayback); ok {
			return vp.PrepareVariant(ctx, user, key, caps, releaseID)
		}
		s.log.Info("variant pick ignored: playback manager has no variant support",
			"key", key, "release", releaseID)
	}
	return s.d.Playback.Prepare(ctx, user, key, caps)
}
