package livetv

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/go-chi/chi/v5"
)

// EPG management: guide-only sources, persisted XMLTV channels, cross-source
// guide mapping, automatic matching, coverage and scheduled source refresh.

func init() {
	store.Register("livetv", 2, `CREATE TABLE livetv_guide_channels(source_id TEXT NOT NULL, guide_id TEXT NOT NULL, name TEXT NOT NULL, logo TEXT NOT NULL DEFAULT '', names TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(source_id, guide_id));
CREATE INDEX livetv_programmes_guide_end ON livetv_programmes(source_id, channel_id, end_ms, start_ms);`)
}

// guideChannelsCTE lists every source's guide channels. Sources imported
// before guide channels were persisted fall back to the distinct programme
// channel ids (named by id) until their next refresh stores the real list.
const guideChannelsCTE = `WITH gc(source_id, guide_id, name, logo, names) AS (
	SELECT source_id, guide_id, name, logo, names FROM livetv_guide_channels
	UNION ALL
	SELECT DISTINCT p.source_id, p.channel_id, p.channel_id, '', '[]' FROM livetv_programmes p
	WHERE NOT EXISTS (SELECT 1 FROM livetv_guide_channels x WHERE x.source_id=p.source_id))
`

const (
	defaultRefreshInterval = 6 * time.Hour
	guideLookback          = 24 * time.Hour
	guideLookahead         = 14 * 24 * time.Hour
	minRefreshInterval     = 30 * time.Minute
	maxRefreshBackoff      = 6 * time.Hour
	guideHorizonMargin     = time.Hour
	refreshCheckInterval   = 5 * time.Minute
	refreshTimeout         = 5 * time.Minute
	// refreshBudget bounds one manual import or automatic refresh. A panel that composes a
	// large guide can take many minutes; the per-document idle timeout ends a
	// stalled fetch, so this only caps a source that keeps sending data.
	refreshBudget  = 30 * time.Minute
	coverageWindow = 24 * time.Hour
)

// GuideChannel is an XMLTV <channel> (or programme channel id) stored for a
// source. Programmes counts programmes that have not yet ended; Until is the
// end of the last stored programme.
type GuideChannel struct {
	SourceID   string     `json:"sourceId"`
	SourceName string     `json:"sourceName"`
	SourceKind string     `json:"sourceKind,omitempty"`
	GuideID    string     `json:"guideId"`
	Name       string     `json:"name"`
	Logo       string     `json:"logo"`
	Programmes int        `json:"programmes"`
	Until      *time.Time `json:"until,omitempty"`
}

type GuideMatchRequest struct {
	Selections    []GuideCommitSelection `json:"selections,omitempty"`
	UndoToken     string                 `json:"undoToken,omitempty"`
	GuideSourceID string                 `json:"guideSourceId,omitempty"`
	AI            string                 `json:"ai,omitempty"` // estimate, propose, or apply; AI never writes without apply.
	ProposalKeys  map[string]string      `json:"proposalKeys,omitempty"`
	ChannelIDs    []string               `json:"channelIds,omitempty"`
	Overwrite     bool                   `json:"overwrite"`
	DryRun        bool                   `json:"dryRun"`
	// Reviewed is set by the owner route for explicitly accepted proposals, never by imports.
	Reviewed bool `json:"-"`
}
type GuideMatchChange struct {
	Confidence      float64 `json:"confidence,omitempty"`
	Reason          string  `json:"reason,omitempty"`
	ProposalKey     string  `json:"proposalKey,omitempty"`
	ChannelID       string  `json:"channelId"`
	ChannelName     string  `json:"channelName"`
	GuideSourceID   string  `json:"guideSourceId"`
	GuideSourceName string  `json:"guideSourceName"`
	GuideID         string  `json:"guideId"`
	GuideName       string  `json:"guideName"`
	Method          string  `json:"method"` // "id", "name" or "alias"
	ReviewRegion    bool    `json:"reviewRegion,omitempty"`
}
type GuideMatchResult struct {
	UndoToken string        `json:"undoToken,omitempty"`
	AI        *GuideAIStats `json:"ai,omitempty"`
	Matched   int           `json:"matched"`
	Unmatched int           `json:"unmatched"`
	Skipped   int           `json:"skipped"` // already had guide data and overwrite was false
	// Ambiguous and RegionRejected count name matches the matcher refused:
	// several guide ids or playlist rows share the name, or the declared
	// regions contradict.
	Ambiguous      int                `json:"ambiguous"`
	RegionRejected int                `json:"regionRejected"`
	DryRun         bool               `json:"dryRun"`
	Changes        []GuideMatchChange `json:"changes"`
}
type GuideCoverage struct {
	ChannelID  string     `json:"channelId"`
	Programmes int        `json:"programmes"`
	Until      *time.Time `json:"until,omitempty"`
}

func (s *Service) guideRoutes(r chi.Router) {
	r.Get("/guide/channels", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		v, e := s.GuideChannels(r.Context(), q.Get("q"), q.Get("source"), limit)
		ownerResult(w, v, e)
	})
	r.Post("/guide/match", func(w http.ResponseWriter, r *http.Request) {
		var req GuideMatchRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err == nil && len(bytes.TrimSpace(body)) > 0 {
			d := json.NewDecoder(bytes.NewReader(body))
			d.DisallowUnknownFields()
			err = d.Decode(&req)
		}
		if err != nil {
			ownerResult(w, nil, fmt.Errorf("invalid request body: %w", err))
			return
		}
		req.Reviewed = len(req.ChannelIDs) > 0 || len(req.Selections) > 0 || req.UndoToken != ""
		v, e := s.MatchGuides(r.Context(), req)
		ownerResult(w, v, e)
	})
	r.Get("/guide/coverage", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Coverage(r.Context())
		ownerResult(w, v, e)
	})
}

// guideSource is the source whose XMLTV supplies an IPTV channel's guide.
func guideSource(c Channel) string {
	if c.GuideSourceID != "" {
		return c.GuideSourceID
	}
	return c.SourceID
}

// GuideChannelLogo returns the logo URL for the channel's matched guide channel,
// or empty string if unmapped or not found.
func (s *Service) GuideChannelLogo(ctx context.Context, c Channel) string {
	if s.o.DB == nil || c.GuideID == "" {
		return ""
	}
	var logo string
	_ = s.o.DB.QueryRowContext(ctx,
		`SELECT logo FROM livetv_guide_channels WHERE source_id=? AND guide_id=?`,
		guideSource(c), c.GuideID).Scan(&logo)
	return logo
}

func msTime(ms int64) *time.Time {
	t := time.UnixMilli(ms).UTC()
	return &t
}

// storedSource is the persisted form: content fetched from a URL is refetched
// on refresh rather than stored, and read-only statistics are dropped.
func storedSource(src Source) Source {
	if src.PlaylistURL != "" {
		src.Playlist = ""
	}
	if src.GuideURL != "" {
		src.Guide = ""
	}
	src.LastError, src.LastErrorAt, src.RefreshFailures = "", nil, 0
	src.HasPlaylist, src.HasGuide = false, false
	src.ChannelCount, src.GuideChannelCount, src.ProgrammeCount = 0, 0, 0
	src.GuideFrom, src.GuideUntil, src.NextRefresh = nil, nil, nil
	return src
}

func (s *Service) storedSourceByID(ctx context.Context, id string) (Source, error) {
	var src Source
	var b []byte
	err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_sources WHERE id=?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return src, core.ErrNotFound
	}
	if err == nil {
		src, _, err = s.decodeSource(b)
	}
	return src, err
}

// keepStoredUploads lets an owner edit a source (for example its name or
// refresh interval) without re-uploading inline files, which GET never returns.
func (s *Service) keepStoredUploads(ctx context.Context, src *Source) error {
	needPlaylist := src.Playlist == "" && src.PlaylistURL == ""
	needGuide := src.Guide == "" && src.GuideURL == ""
	if !needPlaylist && !needGuide {
		return nil
	}
	old, err := s.storedSourceByID(ctx, src.ID)
	if errors.Is(err, core.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if needPlaylist && old.PlaylistURL == "" {
		src.Playlist = old.Playlist
	}
	if needGuide && old.GuideURL == "" {
		src.Guide = old.Guide
	}
	return nil
}

// changedStreams lists previously imported channels whose stream changed or
// which no longer exist after an import.
func changedStreams(old map[string]Channel, cs []Channel) []string {
	now := make(map[string]Channel, len(cs))
	for _, c := range cs {
		now[c.ID] = c
	}
	var out []string
	for id, o := range old {
		if n, ok := now[id]; !ok || n.StreamURL != o.StreamURL || n.Transcode != o.Transcode {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) sourceNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT id, COALESCE(`+store.JSONText(store.DialectOf(s.o.DB), "data", "name")+`,'') FROM livetv_sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// fillSourceStats computes read-only source statistics at read time.
func (s *Service) fillSourceStats(ctx context.Context, srcs []Source) error {
	if len(srcs) == 0 {
		return nil
	}
	type progStats struct {
		n          int
		from, till int64
	}
	all, err := s.channelSummaries(ctx)
	if err != nil {
		return err
	}
	channels := map[string]int{}
	for _, c := range all {
		channels[c.SourceID]++
	}
	guides := map[string]int{}
	progs := map[string]progStats{}
	scan := func(q string, fn func(*sql.Rows) error) error {
		rows, err := s.o.DB.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err = fn(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := scan(guideChannelsCTE+`SELECT source_id, COUNT(*) FROM gc GROUP BY source_id`, func(r *sql.Rows) error {
		var id string
		var n int
		err := r.Scan(&id, &n)
		guides[id] = n
		return err
	}); err != nil {
		return err
	}
	if err := scan(`SELECT source_id, COUNT(*), MIN(start_ms), MAX(end_ms) FROM livetv_programmes GROUP BY source_id`, func(r *sql.Rows) error {
		var id string
		var p progStats
		err := r.Scan(&id, &p.n, &p.from, &p.till)
		progs[id] = p
		return err
	}); err != nil {
		return err
	}
	for i := range srcs {
		src := &srcs[i]
		src.ChannelCount = channels[src.ID]
		src.GuideChannelCount = guides[src.ID]
		src.ProgrammeCount, src.GuideFrom, src.GuideUntil = 0, nil, nil
		if p, ok := progs[src.ID]; ok && p.n > 0 {
			src.ProgrammeCount = p.n
			src.GuideFrom, src.GuideUntil = msTime(p.from), msTime(p.till)
		}
	}
	return s.planRefresh(ctx, srcs)
}

// maxGuideSearchWords bounds the per-word predicates one search builds.
const maxGuideSearchWords = 6

// GuideChannels searches stored XMLTV channels. Every word of q must appear,
// in any order, in the name, a display name or the guide id, so "fox phoenix"
// finds "FOX LOCAL Phoenix". Exact matches rank first, then channels that
// still have listings ahead, then by name.
func (s *Service) GuideChannels(ctx context.Context, q, source string, limit int) ([]GuideChannel, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q = strings.ToLower(strings.TrimSpace(q))
	names, err := s.sourceNames(ctx)
	if err != nil {
		return nil, err
	}
	kinds := map[string]string{}
	d := store.DialectOf(s.o.DB)
	sourceRows, err := s.o.DB.QueryContext(ctx, `SELECT id, `+store.JSONText(d, "data", "kind")+` FROM livetv_sources`)
	if err != nil {
		return nil, err
	}
	for sourceRows.Next() {
		var id string
		var kind sql.NullString
		if err = sourceRows.Scan(&id, &kind); err != nil {
			break
		}
		kinds[id] = kind.String
	}
	if err == nil {
		err = sourceRows.Err()
	}
	sourceRows.Close()
	if err != nil {
		return nil, err
	}
	now := s.o.Now().UnixMilli()
	args := []any{now, source, q, limit}
	words := strings.Fields(q)
	if len(words) > maxGuideSearchWords {
		words = words[:maxGuideSearchWords]
	}
	match := ""
	for _, w := range words {
		args = append(args, w)
		p := fmt.Sprintf("?%d", len(args))
		match += ` AND (` + store.Contains(d, "lower(g.name)", p) + ` OR ` + store.Contains(d, "lower(g.guide_id)", p) + ` OR ` + store.Contains(d, "lower(g.names)", p) + `)`
	}
	rows, err := s.o.DB.QueryContext(ctx, guideChannelsCTE+`SELECT source_id, guide_id, name, logo, ahead, until_ms FROM (
		SELECT g.source_id, g.guide_id, g.name, g.logo,
			(SELECT COUNT(*) FROM livetv_programmes p WHERE p.source_id=g.source_id AND p.channel_id=g.guide_id AND p.end_ms>?1) AS ahead,
			(SELECT MAX(end_ms) FROM livetv_programmes p WHERE p.source_id=g.source_id AND p.channel_id=g.guide_id) AS until_ms,
			CASE WHEN lower(g.guide_id)=?3 OR lower(g.name)=?3 THEN 1 ELSE 0 END AS exact,
			lower(g.name) AS sort_name
		FROM gc g
		WHERE (?2='' OR g.source_id=?2)`+match+`) m
		ORDER BY exact DESC, CASE WHEN ahead>0 THEN 1 ELSE 0 END DESC, sort_name, source_id, guide_id LIMIT ?4`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GuideChannel{}
	for rows.Next() {
		var g GuideChannel
		var until sql.NullInt64
		if err = rows.Scan(&g.SourceID, &g.GuideID, &g.Name, &g.Logo, &g.Programmes, &until); err != nil {
			return nil, err
		}
		g.SourceName = names[g.SourceID]
		g.SourceKind = kinds[g.SourceID]
		if until.Valid {
			g.Until = msTime(until.Int64)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

type guideKey struct{ source, guide string }
type guideStat struct {
	window int   // programmes overlapping [from, until)
	until  int64 // end of the last programme ending after from
}

// guideStats summarizes every stored guide channel with programmes after from.
func (s *Service) guideStats(ctx context.Context, from, until time.Time) (map[guideKey]guideStat, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT source_id, channel_id, SUM(CASE WHEN start_ms<? THEN 1 ELSE 0 END), MAX(end_ms) FROM livetv_programmes WHERE end_ms>? GROUP BY source_id, channel_id`, until.UnixMilli(), from.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[guideKey]guideStat{}
	for rows.Next() {
		var k guideKey
		var v guideStat
		if err = rows.Scan(&k.source, &k.guide, &v.window, &v.until); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Coverage reports, for every channel, how many programmes the guide has in
// the next 24 hours and when its guide data runs out.
func (s *Service) Coverage(ctx context.Context) ([]GuideCoverage, error) {
	all, err := s.channelSummaries(ctx)
	if err != nil {
		return nil, err
	}
	from := s.o.Now()
	until := from.Add(coverageWindow)
	virtualIDs := make([]string, 0)
	for _, c := range all {
		if c.Kind == "virtual" {
			virtualIDs = append(virtualIDs, c.ID)
		}
	}
	virtual, err := s.channelsByIDs(ctx, virtualIDs)
	if err != nil {
		return nil, err
	}
	stats, err := s.guideCoverageStats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GuideCoverage, 0, len(all))
	for _, c := range all {
		v := GuideCoverage{ChannelID: c.ID}
		if c.Kind == "virtual" {
			ps := Schedule(virtual[c.ID], from, until)
			v.Programmes = len(ps)
			if len(ps) > 0 {
				end := ps[len(ps)-1].End.UTC()
				v.Until = &end
			}
		} else if st, ok := stats[guideKey{summaryGuideSource(c), c.GuideID}]; ok && c.GuideID != "" {
			v.Programmes = st.window
			v.Until = msTime(st.until)
		}
		out = append(out, v)
	}
	return out, nil
}

var (
	guideCountryPrefix = regexp.MustCompile(`^[\s|]*([\p{L}]{2,3})\s*[:|]\s*`)
	guideBracketed     = regexp.MustCompile(`[\[(]([^\])]*)[\])]`)
	guideQuality       = map[string]bool{"hd": true, "fhd": true, "uhd": true, "4k": true, "8k": true, "sd": true, "hq": true, "lq": true, "hevc": true, "h264": true, "h265": true, "1080p": true, "1080i": true, "720p": true, "2160p": true, "50fps": true, "60fps": true}
)

// guideRegionCodes is the ISO 3166-1 alpha-2 set (plus "uk" and "usa") used to
// recognise the region a name declares. The generic pseudo-TLDs .tv, .fm,
// .io, .me, .cc and .ws are deliberately absent: they are words in channel
// names and suffixes in fake ids, not regions.
var guideRegionCodes = func() map[string]string {
	codes := strings.Fields(`
		ad ae af ag ai al am ao aq ar as at au aw ax az
		ba bb bd be bf bg bh bi bj bl bm bn bo bq br bs bt bv bw by bz
		ca cd cf cg ch ci ck cl cm cn co cr cu cv cw cx cy cz
		de dj dk dm do dz
		ec ee eg eh er es et
		fi fj fk fo fr
		ga gb gd ge gf gg gh gi gl gm gn gp gq gr gs gt gu gw gy
		hk hm hn hr ht hu
		id ie il im in iq ir is it
		je jm jo jp
		ke kg kh ki km kn kp kr kw ky kz
		la lb lc li lk lr ls lt lu lv ly
		ma mc md mf mg mh mk ml mm mn mo mp mq mr ms mt mu mv mw mx my mz
		na nc ne nf ng ni nl no np nr nu nz
		om
		pa pe pf pg ph pk pl pm pn pr ps pt pw py
		qa
		re ro rs ru rw
		sa sb sc sd se sg sh si sj sk sl sm sn so sr ss st sv sx sy sz
		tc td tf tg th tj tk tl tm tn to tr tt tw tz
		ua ug um us uy uz
		va vc ve vg vi vn vu
		wf
		ye yt
		za zm zw
		uk usa`)
	m := make(map[string]string, len(codes))
	for _, c := range codes {
		m[c] = strings.ToUpper(c)
	}
	m["uk"] = "GB"
	return m
}()

// guideNameRegion folds a channel name for matching and reports the region it
// declares, if any: a leading prefix ("US:", "|UK|", "UK |"), a bracketed
// tag ("(US)"), or an upper-case trailing country code ("News US"). Case,
// punctuation, quality tokens and such region labels are ignored; "+" is kept
// so "+1" timeshifts stay distinct.
func guideNameRegion(name string) (region, normalized string) {
	s := strings.TrimSpace(name)
	if m := guideCountryPrefix.FindStringSubmatch(s); m != nil {
		if r, ok := guideRegionCodes[strings.ToLower(m[1])]; ok {
			region = r
		}
		// Any short namespace prefix is folded away as before; only a known
		// country code labels the region.
		s = s[len(m[0]):]
	}
	s = guideBracketed.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(m[1 : len(m)-1])
		lower := strings.ToLower(inner)
		if label, ok := upperGuideToken(inner); ok {
			if r, ok := guideRegionCodes[label]; ok && region == "" {
				region = r
			}
		}
		if guideQuality[lower] || len([]rune(inner)) <= 3 && !strings.Contains(inner, "+") {
			return " "
		}
		return " " + inner + " "
	})
	tokens := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' })
	kept := tokens[:0]
	for _, t := range tokens {
		if !guideQuality[strings.ToLower(t)] {
			kept = append(kept, t)
		}
	}
	if n := len(kept); n > 1 {
		// A trailing country code labels the name before it. A lone token
		// like "SC", or an ordinary word like "It", is not a region.
		if label, ok := upperGuideToken(kept[n-1]); ok {
			if r, ok := guideRegionCodes[label]; ok {
				if region == "" {
					region = r
				}
				kept = kept[:n-1]
			}
		}
	}
	var b strings.Builder
	for _, t := range kept {
		b.WriteString(strings.ToLower(t))
	}
	return region, b.String()
}

// upperGuideToken reports a token written entirely in upper case, the way a
// region label is printed ("US", "UK", "USA"). Ordinary words are title or
// sentence case and are not mistaken for country codes.
func upperGuideToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	for _, r := range token {
		if !unicode.IsUpper(r) {
			return "", false
		}
	}
	return strings.ToLower(token), true
}

func normalizeGuideName(name string) string {
	_, normalized := guideNameRegion(name)
	return normalized
}

// guideIDRegion reports the region a guide id declares at its end
// ("News1.de", "Public1.uk") or through a leading prefix. Unknown suffixes and
// the generic pseudo-TLDs are ignored.
func guideIDRegion(id string) string {
	id = strings.TrimSpace(id)
	if region, _ := guideNameRegion(id); region != "" {
		return region
	}
	if i := strings.LastIndexByte(id, '.'); i >= 0 {
		if r, ok := guideRegionCodes[strings.ToLower(strings.TrimSpace(id[i+1:]))]; ok {
			return r
		}
	}
	return ""
}

// channelRegion reports the region a playlist channel declares: its name or
// tvg-name prefix/trailing label first, then its group-title, then its
// current guide id's suffix.
func channelRegion(c Channel) string {
	for _, name := range []string{c.Name, c.TvgName} {
		if region, _ := guideNameRegion(name); region != "" {
			return region
		}
	}
	if region, _ := guideNameRegion(c.Group); region != "" {
		return region
	}
	return guideIDRegion(c.GuideID)
}

// guideCandidateRegion reports the region a guide channel declares: its id's
// suffix first, then its first display name that carries one.
func guideCandidateRegion(id, name string, names []string) string {
	if region := guideIDRegion(id); region != "" {
		return region
	}
	for _, n := range append([]string{name}, names...) {
		if region, _ := guideNameRegion(n); region != "" {
			return region
		}
	}
	return ""
}

type guideCandidate struct {
	source, guide, name, region string
	aliases                     []string
	window                      int
	until                       int64
}

// MatchGuides maps IPTV channels to guide channels from any source: an exact
// tvg-id wins, then a case-insensitive id, then a normalized display name.
// Only guide channels with upcoming programmes are considered, and disabled
// channels only when listed in ChannelIDs. Without
// Overwrite, channels that already have programmes in the next 24 hours are
// left alone.
func (s *Service) MatchGuides(ctx context.Context, req GuideMatchRequest) (GuideMatchResult, error) {
	if req.AI == "commit" || req.AI == "undo" {
		return s.commitGuideReview(ctx, req)
	}
	if req.AI != "" && req.AI != "estimate" && req.AI != "propose" && req.AI != "apply" {
		return GuideMatchResult{}, fmt.Errorf("invalid AI operation")
	}
	if req.AI == "estimate" || req.AI == "propose" {
		req.DryRun = true
	}
	if req.AI == "apply" && (len(req.ChannelIDs) == 0 || !req.Reviewed) {
		return GuideMatchResult{}, fmt.Errorf("AI apply requires reviewed channel ids")
	}
	res := GuideMatchResult{DryRun: req.DryRun, Changes: []GuideMatchChange{}}
	cs, err := s.allChannels(ctx)
	if err != nil {
		return res, err
	}
	from := s.o.Now()
	stats, err := s.guideStats(ctx, from, from.Add(coverageWindow))
	if err != nil {
		return res, err
	}
	sourceNames, err := s.sourceNames(ctx)
	if err != nil {
		return res, err
	}
	byID := map[string][]*guideCandidate{}
	byFold := map[string][]*guideCandidate{}
	byName := map[string][]*guideCandidate{}
	byAlias := map[string][]*guideCandidate{}
	remoteSources := map[string]bool{}
	sourceRows, err := s.o.DB.QueryContext(ctx, `SELECT id, data FROM livetv_sources`)
	if err != nil {
		return res, err
	}
	for sourceRows.Next() {
		var id string
		var data []byte
		var source Source
		if err = sourceRows.Scan(&id, &data); err != nil {
			break
		}
		if json.Unmarshal(data, &source) == nil && s.sourceGuideFeatures(source).ReviewedAliases {
			remoteSources[id] = true
		}
	}
	if err == nil {
		err = sourceRows.Err()
	}
	sourceRows.Close()
	if err != nil {
		return res, err
	}
	rows, err := s.o.DB.QueryContext(ctx, guideChannelsCTE+`SELECT source_id, guide_id, name, names FROM gc`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var src, id, name, raw string
		if err = rows.Scan(&src, &id, &name, &raw); err != nil {
			rows.Close()
			return res, err
		}
		if req.AI != "" && req.GuideSourceID != "" && src != req.GuideSourceID {
			continue
		}
		st, ok := stats[guideKey{src, id}]
		if !ok && !remoteSources[src] {
			continue
		}
		cand := &guideCandidate{source: src, guide: id, name: name, window: st.window, until: st.until}
		byID[id] = append(byID[id], cand)
		fold := strings.ToLower(id)
		byFold[fold] = append(byFold[fold], cand)
		var all []string
		json.Unmarshal([]byte(raw), &all)
		cand.aliases = all
		cand.region = guideCandidateRegion(id, name, all)
		seen := map[string]bool{}
		for _, n := range append(all, name) {
			if remoteSources[src] {
				if k := normalizeGuideName(n); k != "" {
					byAlias[k] = append(byAlias[k], cand)
				}
				continue
			}
			if k := normalizeGuideName(n); k != "" && !seen[k] {
				seen[k] = true
				byName[k] = append(byName[k], cand)
			}
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return res, err
	}
	// A name shared by more than one playlist row is not a key; count those
	// rows so a name match can refuse instead of picking one.
	nameCount := map[string]int{}
	for _, c := range cs {
		if c.Kind != "iptv" {
			continue
		}
		seen := map[string]bool{}
		for _, n := range []string{c.TvgName, c.Name} {
			k := normalizeGuideName(n)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			nameCount[k]++
		}
	}
	want := map[string]bool{}
	for _, id := range req.ChannelIDs {
		want[id] = true
	}
	var changed []Channel
	var unresolved []Channel
	for _, c := range cs {
		// Off-air channels are only matched when asked for by id; otherwise a
		// lineup with retired channels floods the proposals.
		if c.Kind != "iptv" || len(want) > 0 && !want[c.ID] || len(want) == 0 && !c.Enabled {
			continue
		}
		if req.AI == "apply" && req.ProposalKeys[c.ID] != "" {
			unresolved = append(unresolved, c)
			res.Unmatched++
			continue
		}
		if !req.Overwrite && c.GuideID != "" && stats[guideKey{guideSource(c), c.GuideID}].window > 0 {
			res.Skipped++
			continue
		}
		var pick *guideCandidate
		var rejected string
		method := "id"
		if c.GuideID != "" {
			pick = bestCandidate(c, byID[c.GuideID])
			if pick == nil {
				pick = bestCandidate(c, byFold[strings.ToLower(c.GuideID)])
			}
		}
		if pick == nil {
			method = "name"
			pick, rejected = nameCandidateForChannel(c, byName, nameCount, req.DryRun || req.Reviewed)
			switch rejected {
			case "ambiguous":
				res.Ambiguous++
			case "region":
				res.RegionRejected++
			}
		}
		if pick == nil && rejected != "ambiguous" && rejected != "region" && (req.DryRun || req.Reviewed) {
			method = "alias"
			normalized := normalizeGuideName(c.Name)
			pick = bestCandidate(c, byAlias[normalized])
			if pick == nil && len(normalized) >= 4 {
				var aliases []string
				for alias := range byAlias {
					if strings.HasPrefix(alias, normalized) {
						aliases = append(aliases, alias)
					}
				}
				sort.Slice(aliases, func(i, j int) bool {
					if len(aliases[i]) != len(aliases[j]) {
						return len(aliases[i]) < len(aliases[j])
					}
					return aliases[i] < aliases[j]
				})
				if len(aliases) > 0 {
					pick = bestCandidate(c, byAlias[aliases[0]])
				}
			}
			if pick == nil {
				for _, word := range strings.FieldsFunc(strings.ToLower(c.Name), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
					if len(word) >= 3 {
						if candidate := bestCandidate(c, byAlias[word]); candidate != nil {
							pick = candidate
							break
						}
					}
				}
			}
		}
		if pick == nil {
			res.Unmatched++
			unresolved = append(unresolved, c)
			continue
		}
		res.Matched++
		if pick.source == guideSource(c) && pick.guide == c.GuideID {
			continue
		}
		res.Changes = append(res.Changes, GuideMatchChange{ChannelID: c.ID, ChannelName: c.Name, GuideSourceID: pick.source, GuideSourceName: sourceNames[pick.source], GuideID: pick.guide, GuideName: pick.name, Method: method, ReviewRegion: method != "id" && (channelRegion(c) == "" || pick.region == "")})
		c.GuideID = pick.guide
		c.GuideSourceID = pick.source
		if pick.source == c.SourceID {
			c.GuideSourceID = ""
		}
		changed = append(changed, c)
	}
	if req.AI != "" {
		var candidates []*guideCandidate
		for _, list := range byID {
			candidates = append(candidates, list...)
		}
		extra, e := s.aiGuideMatches(ctx, req, unresolved, candidates, sourceNames, &res)
		if e != nil {
			return res, e
		}
		changed = append(changed, extra...)
	}
	if req.DryRun || len(changed) == 0 {
		return res, nil
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, c := range changed {
		b, err := json.Marshal(c)
		if err != nil {
			return res, err
		}
		// Only the mapping changes; active tunes keep playing.
		if req.AI == "apply" {
			var raw []byte
			if err = tx.QueryRowContext(ctx, `SELECT data FROM livetv_channels WHERE id=?`, c.ID).Scan(&raw); err != nil {
				return res, err
			}
			var current Channel
			if err = json.Unmarshal(raw, &current); err != nil {
				return res, err
			}
			for _, original := range cs {
				if original.ID == c.ID && aiHash(current) != aiHash(original) {
					return res, fmt.Errorf("channel changed; review again")
				}
			}
			updated, e := tx.ExecContext(ctx, `UPDATE livetv_channels SET data=? WHERE id=? AND CAST(data AS TEXT)=?`, string(b), c.ID, string(raw))
			if e != nil {
				return res, e
			}
			n, e := updated.RowsAffected()
			if e != nil {
				return res, e
			}
			if n != 1 {
				return res, fmt.Errorf("channel changed; review again")
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE livetv_channels SET data=? WHERE id=?`, b, c.ID); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	s.wakeSourceRefresh()
	return res, nil
}

// channelGuideNames lists the normalized labels a playlist channel can match,
// provider tvg-name first, then its display name.
func channelGuideNames(c Channel) []string {
	out := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, n := range []string{c.TvgName, c.Name} {
		k := normalizeGuideName(n)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// uniqueGuideCandidates drops duplicate (source, guide) rows.
func uniqueGuideCandidates(cands []*guideCandidate) []*guideCandidate {
	seen := map[guideKey]bool{}
	out := make([]*guideCandidate, 0, len(cands))
	for _, c := range cands {
		k := guideKey{c.source, c.guide}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	return out
}

// nameCandidateForChannel applies the name-matching rules and reports why it
// refused a match. A candidate from the channel's own source wins over other
// sources, so a provider's own guide is preferred. Within that pool there
// must be one guide id and one playlist channel with the normalized name. A
// declared region that contradicts is always rejected; one unknown region is
// only a proposal until the owner explicitly confirms it.
func nameCandidateForChannel(c Channel, byName map[string][]*guideCandidate, nameCount map[string]int, dryRun bool) (*guideCandidate, string) {
	for _, k := range channelGuideNames(c) {
		cands := uniqueGuideCandidates(byName[k])
		if len(cands) == 0 {
			continue
		}
		own := make([]*guideCandidate, 0, len(cands))
		for _, g := range cands {
			if g.source == c.SourceID {
				own = append(own, g)
			}
		}
		if len(own) > 0 {
			cands = own
		}
		ids := map[string]bool{}
		for _, g := range cands {
			ids[g.guide] = true
		}
		if len(ids) > 1 || nameCount[k] > 1 {
			return nil, "ambiguous"
		}
		pick := bestNameCandidate(c, cands)
		cr, gr := channelRegion(c), pick.region
		if cr != "" && gr != "" && cr != gr {
			return nil, "region"
		}
		if (cr == "" || gr == "") && !dryRun {
			return nil, "review"
		}
		return pick, ""
	}
	return nil, ""
}

// bestNameCandidate picks between candidates that all name the same guide id.
// It never uses programme counts: a name tie is an ambiguity, not a
// preference. Only source and the current mapping rank, then ids break ties.
func bestNameCandidate(c Channel, cands []*guideCandidate) *guideCandidate {
	best := cands[0]
	for _, g := range cands[1:] {
		if a, b := nameCandidateRank(c, g), nameCandidateRank(c, best); a != b {
			if a > b {
				best = g
			}
			continue
		}
		if g.source < best.source || g.source == best.source && g.guide < best.guide {
			best = g
		}
	}
	return best
}

func nameCandidateRank(c Channel, g *guideCandidate) int {
	switch {
	case g.source == c.SourceID:
		return 2
	case g.source == guideSource(c) && g.guide == c.GuideID:
		return 1
	}
	return 0
}

// bestCandidate prefers guides with programmes in the next 24 hours, then the
// channel's own source, then its current mapping, then the fullest guide.
func bestCandidate(c Channel, cands []*guideCandidate) *guideCandidate {
	if len(cands) == 0 {
		return nil
	}
	rank := func(g *guideCandidate) [3]bool {
		return [3]bool{g.window > 0, g.source == c.SourceID, g.source == guideSource(c) && g.guide == c.GuideID}
	}
	best := cands[0]
	for _, g := range cands[1:] {
		a, b := rank(g), rank(best)
		better := false
		decided := false
		for i := range a {
			if a[i] != b[i] {
				better, decided = a[i], true
				break
			}
		}
		if !decided {
			switch {
			case g.window != best.window:
				better = g.window > best.window
			case g.until != best.until:
				better = g.until > best.until
			case g.source != best.source:
				better = g.source < best.source
			default:
				better = g.guide < best.guide
			}
		}
		if better {
			best = g
		}
	}
	return best
}

// refreshInterval reports the longest time between automatic refreshes. Only
// URL-backed sources refresh; inline uploads have nothing new to fetch.
func refreshInterval(src Source) (time.Duration, bool) {
	if src.Disabled {
		return 0, false
	}
	if !defaultGuideFeatures(src).Remote && src.PlaylistURL == "" && src.GuideURL == "" || src.RefreshHours < 0 {
		return 0, false
	}
	if src.RefreshHours == 0 {
		return defaultRefreshInterval, true
	}
	return time.Duration(min(src.RefreshHours, 24*365)) * time.Hour, true
}

// nextRefresh is the next automatic refresh: the refresh interval, brought
// forward to an hour before the first mapped guide channel's listings run out
// (horizon), but never sooner than 30 minutes after the last refresh. After
// failures it backs off exponentially: 30m, 1h, 2h, ... capped at 6h.
func nextRefresh(src Source, horizon *time.Time) (time.Time, bool) {
	iv, ok := refreshInterval(src)
	if !ok {
		return time.Time{}, false
	}
	if src.LastErrorAt != nil && src.LastErrorAt.After(src.UpdatedAt) {
		backoff := maxRefreshBackoff
		if n := max(src.RefreshFailures, 1); n <= 5 {
			backoff = min(minRefreshInterval<<(n-1), maxRefreshBackoff)
		}
		return src.LastErrorAt.Add(backoff).UTC(), true
	}
	next := src.UpdatedAt.Add(iv)
	if horizon != nil && (src.GuideURL != "" || defaultGuideFeatures(src).Remote) {
		if h := horizon.Add(-guideHorizonMargin); h.Before(next) {
			next = h
		}
	}
	if floor := src.UpdatedAt.Add(minRefreshInterval); next.Before(floor) {
		next = floor
	}
	return next.UTC(), true
}

// planRefresh sets NextRefresh on each source. The horizon of a source is the
// earliest last-programme end among its guide channels that an enabled IPTV
// channel uses, ignoring channels whose listings had already ended when the
// source was last imported (refreshing would not extend them).
func (s *Service) planRefresh(ctx context.Context, srcs []Source) error {
	if err := s.overlayRefreshStamps(ctx, srcs); err != nil {
		return err
	}
	cs, err := s.Channels(ctx)
	if err != nil {
		return err
	}
	used := map[guideKey]bool{}
	for _, c := range cs {
		if c.Kind == "iptv" && c.Enabled && c.GuideID != "" {
			used[guideKey{guideSource(c), c.GuideID}] = true
		}
	}
	updated := map[string]int64{}
	for _, src := range srcs {
		updated[src.ID] = src.UpdatedAt.UnixMilli()
	}
	horizons := map[string]int64{}
	covered := map[guideKey]bool{}
	if len(used) > 0 {
		rows, err := s.o.DB.QueryContext(ctx, `SELECT source_id, channel_id, MAX(end_ms) FROM livetv_programmes GROUP BY source_id, channel_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k guideKey
			var end int64
			if err = rows.Scan(&k.source, &k.guide, &end); err != nil {
				rows.Close()
				return err
			}
			if end > s.o.Now().UnixMilli() {
				covered[k] = true
			}
			at, ok := updated[k.source]
			if !ok || !used[k] || end <= at {
				continue
			}
			if h, ok := horizons[k.source]; !ok || end < h {
				horizons[k.source] = end
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
	}
	for i := range srcs {
		var horizon *time.Time
		if h, ok := horizons[srcs[i].ID]; ok {
			horizon = msTime(h)
		}
		srcs[i].NextRefresh = nil
		if next, ok := nextRefresh(srcs[i], horizon); ok {
			if s.sourceGuideFeatures(srcs[i]).Remote && (srcs[i].LastErrorAt == nil || !srcs[i].LastErrorAt.After(srcs[i].UpdatedAt)) {
				for key := range used {
					if key.source == srcs[i].ID && !covered[key] {
						if earlier := srcs[i].UpdatedAt.Add(minRefreshInterval); earlier.Before(next) {
							next = earlier
						}
						break
					}
				}
			}
			srcs[i].NextRefresh = &next
		}
	}
	return nil
}

// RefreshSource re-imports a stored source. On failure the existing channels
// and guide are kept and the error is recorded on the source.
func (s *Service) RefreshSource(ctx context.Context, id string) (Source, error) {
	return s.refreshSource(ctx, id, "manual")
}

func (s *Service) refreshSource(ctx context.Context, id, reason string) (Source, error) {
	if reason != "manual" && !s.Enabled() {
		return Source{}, core.ErrNotAvailable
	}
	ctx, cancel := context.WithTimeout(ctx, refreshBudget)
	defer cancel()
	release, err := s.acquireRefresh(ctx, id)
	if err != nil {
		return Source{}, err
	}
	defer release()
	src, err := s.storedSourceByID(ctx, id)
	if err != nil {
		return src, err
	}
	if src.Disabled {
		return src, core.ErrNotAvailable
	}
	// Recheck after admission: another node may have completed the refresh
	// since this worker selected the source.
	current := []Source{src}
	if reason != "manual" {
		if err := s.planRefresh(ctx, current); err != nil {
			return src, err
		}
		src = current[0]
		due := src.NextRefresh != nil && !s.o.Now().Before(*src.NextRefresh)
		if reason == "activity" {
			due = activityRefreshDue(src, s.o.Now())
		}
		if !due {
			return src, nil
		}
	}
	s.o.Logger.Debug("Live TV source refresh", "sourceId", id, "reason", reason)
	v, err := s.importSourceLogged(ctx, src)
	if err != nil {
		at := s.o.Now().UTC().Format(time.RFC3339Nano)
		msg := err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		q := `UPDATE livetv_sources SET data=json_set(data,'$.lastError',?,'$.lastErrorAt',?,'$.refreshFailures',COALESCE(json_extract(data,'$.refreshFailures'),0)+1) WHERE id=?`
		if store.DialectOf(s.o.DB) == store.Postgres {
			q = `UPDATE livetv_sources SET data=(data::jsonb || jsonb_build_object('lastError', CAST(? AS TEXT), 'lastErrorAt', CAST(? AS TEXT),
				'refreshFailures', COALESCE(CAST(data::jsonb->>'refreshFailures' AS BIGINT),0)+1))::text WHERE id=?`
		}
		errCtx, errCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer errCancel()
		if _, e := s.o.DB.ExecContext(errCtx, q, msg, at, id); e != nil {
			return v, errors.Join(err, e)
		}
		s.notifySourcesChanged()
	}
	return v, err
}

// refreshDueSources refreshes every source whose automatic refresh is due and
// returns the ids it attempted.
func (s *Service) refreshDueSources(ctx context.Context) []string {
	return s.refreshSources(ctx, false)
}

func (s *Service) wakeSourceRefresh() {
	s.noActivitySources.Store(false)
	if s.sourceWake != nil {
		select {
		case s.sourceWake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) loadScheduledSources(ctx context.Context) ([]Source, error) {
	// Only scheduling fields are read; inline uploads can be large.
	d := store.DialectOf(s.o.DB)
	rows, err := s.o.DB.QueryContext(ctx, `SELECT id, `+store.JSONText(d, "data", "kind")+`, `+store.JSONText(d, "data", "playlistUrl")+`, `+store.JSONText(d, "data", "guideUrl")+`, `+store.JSONInt(d, "data", "refreshHours")+`, `+store.JSONText(d, "data", "updatedAt")+`, `+store.JSONText(d, "data", "lastErrorAt")+`, `+store.JSONInt(d, "data", "refreshFailures")+`, `+store.JSONInt(d, "data", "activityRefreshMinutes")+` FROM livetv_sources WHERE COALESCE(CAST(`+store.JSONText(d, "data", "disabled")+` AS TEXT), 'false') NOT IN ('true', '1') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var srcs []Source
	for rows.Next() {
		var id string
		var kind, playlist, guide, updated, failed sql.NullString
		var hours, failures, activityMinutes sql.NullInt64
		if rows.Scan(&id, &kind, &playlist, &guide, &hours, &updated, &failed, &failures, &activityMinutes) != nil {
			continue
		}
		src := Source{ID: id, Kind: kind.String, PlaylistURL: playlist.String, GuideURL: guide.String, RefreshHours: int(hours.Int64), RefreshFailures: int(failures.Int64)}
		src.ActivityRefreshMinutes = int(activityMinutes.Int64)
		src.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated.String)
		if t, e := time.Parse(time.RFC3339Nano, failed.String); e == nil {
			src.LastErrorAt = &t
		}
		srcs = append(srcs, src)
	}
	return srcs, rows.Err()
}

func (s *Service) refreshSources(ctx context.Context, activity bool) []string {
	srcs, err := s.loadScheduledSources(ctx)
	if err != nil || s.planRefresh(ctx, srcs) != nil {
		return nil
	}
	now := s.o.Now()
	var due []string
	for _, src := range srcs {
		if (!activity && src.NextRefresh != nil && !now.Before(*src.NextRefresh)) || (activity && activityRefreshDue(src, now)) {
			due = append(due, src.ID)
		}
	}
	for _, id := range due {
		if ctx.Err() != nil {
			break
		}
		rctx, cancel := context.WithTimeout(ctx, refreshBudget)
		reason := "scheduled"
		if activity {
			reason = "activity"
		}
		s.refreshSource(rctx, id, reason)
		cancel()
	}
	return due
}

func (s *Service) nextSourceRefreshWait(ctx context.Context) (time.Duration, bool) {
	srcs, err := s.loadScheduledSources(ctx)
	if err != nil {
		return refreshCheckInterval, true
	}
	standalone := store.DialectOf(s.o.DB) == store.SQLite
	if standalone {
		anyActivity := false
		for _, src := range srcs {
			if _, enabled := refreshInterval(src); enabled && src.ActivityRefreshMinutes > 0 {
				anyActivity = true
				break
			}
		}
		s.noActivitySources.Store(!anyActivity)
		if len(srcs) == 0 {
			return 0, false
		}
	} else if len(srcs) == 0 {
		return refreshCheckInterval, true
	}
	if err := s.planRefresh(ctx, srcs); err != nil {
		return refreshCheckInterval, true
	}
	var earliest time.Time
	for _, src := range srcs {
		if src.NextRefresh != nil && (earliest.IsZero() || src.NextRefresh.Before(earliest)) {
			earliest = *src.NextRefresh
		}
	}
	if earliest.IsZero() {
		if standalone {
			return 0, false
		}
		return refreshCheckInterval, true
	}
	wait := earliest.Sub(s.o.Now())
	if wait < time.Second {
		wait = time.Second
	}
	if !standalone && wait > refreshCheckInterval {
		wait = refreshCheckInterval
	}
	return wait, true
}

func (s *Service) refreshLoop() {
	defer s.wg.Done()
	timer := s.newTimer(refreshCheckInterval)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()
	for {
		wait, armed := s.nextSourceRefreshWait(s.ctx)
		if !armed {
			select {
			case <-s.ctx.Done():
				return
			case <-s.sourceWake:
			case <-s.activityWake:
				if !s.noActivitySources.Load() {
					s.refreshSources(s.ctx, true)
				}
			}
			continue
		}
		timer.Reset(wait)
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			s.refreshDueSources(s.ctx)
		case <-s.sourceWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-s.activityWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if !s.noActivitySources.Load() {
				s.refreshSources(s.ctx, true)
			}
		}
	}
}
