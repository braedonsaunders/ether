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
	coverageWindow         = 24 * time.Hour
)

// GuideChannel is an XMLTV <channel> (or programme channel id) stored for a
// source. Programmes counts programmes that have not yet ended; Until is the
// end of the last stored programme.
type GuideChannel struct {
	SourceID   string     `json:"sourceId"`
	SourceName string     `json:"sourceName"`
	GuideID    string     `json:"guideId"`
	Name       string     `json:"name"`
	Logo       string     `json:"logo"`
	Programmes int        `json:"programmes"`
	Until      *time.Time `json:"until,omitempty"`
}

type GuideMatchRequest struct {
	ChannelIDs []string `json:"channelIds,omitempty"`
	Overwrite  bool     `json:"overwrite"`
	DryRun     bool     `json:"dryRun"`
}
type GuideMatchChange struct {
	ChannelID       string `json:"channelId"`
	ChannelName     string `json:"channelName"`
	GuideSourceID   string `json:"guideSourceId"`
	GuideSourceName string `json:"guideSourceName"`
	GuideID         string `json:"guideId"`
	GuideName       string `json:"guideName"`
	Method          string `json:"method"` // "id" or "name"
}
type GuideMatchResult struct {
	Matched   int                `json:"matched"`
	Unmatched int                `json:"unmatched"`
	Skipped   int                `json:"skipped"` // already had guide data and overwrite was false
	DryRun    bool               `json:"dryRun"`
	Changes   []GuideMatchChange `json:"changes"`
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
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil && len(bytes.TrimSpace(body)) > 0 {
			d := json.NewDecoder(bytes.NewReader(body))
			d.DisallowUnknownFields()
			err = d.Decode(&req)
		}
		if err != nil {
			ownerResult(w, nil, fmt.Errorf("invalid request body: %w", err))
			return
		}
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

// replaceGuideChannels stores a source's XMLTV channels inside the import
// transaction. Programme channel ids without a <channel> entry are kept too.
func replaceGuideChannels(ctx context.Context, tx *sql.Tx, sourceID string, g XMLGuide) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM livetv_guide_channels WHERE source_id=?`, sourceID); err != nil {
		return err
	}
	for id, c := range g.Channels {
		if id == "" {
			continue
		}
		names := g.Names[id]
		if names == nil {
			names = []string{}
		}
		b, err := json.Marshal(names)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = id
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_guide_channels(source_id,guide_id,name,logo,names) VALUES(?,?,?,?,?)
			ON CONFLICT(source_id,guide_id) DO UPDATE SET name=excluded.name, logo=excluded.logo, names=excluded.names`, sourceID, id, name, c.Logo, string(b)); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, p := range g.Programmes {
		if _, ok := g.Channels[p.ChannelID]; ok || seen[p.ChannelID] || p.ChannelID == "" {
			continue
		}
		seen[p.ChannelID] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO livetv_guide_channels(source_id,guide_id,name) VALUES(?,?,?) ON CONFLICT DO NOTHING`, sourceID, p.ChannelID, p.ChannelID); err != nil {
			return err
		}
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
	channels := map[string]int{}
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
	if err := scan(`SELECT COALESCE(`+store.JSONText(store.DialectOf(s.o.DB), "data", "sourceId")+`,''), COUNT(*) FROM livetv_channels GROUP BY 1`, func(r *sql.Rows) error {
		var id string
		var n int
		err := r.Scan(&id, &n)
		channels[id] = n
		return err
	}); err != nil {
		return err
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

// GuideChannels searches stored XMLTV channels by case-insensitive substring
// of name, any display name, or guide id.
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
	now := s.o.Now().UnixMilli()
	d := store.DialectOf(s.o.DB)
	rows, err := s.o.DB.QueryContext(ctx, guideChannelsCTE+`SELECT g.source_id, g.guide_id, g.name, g.logo,
		(SELECT COUNT(*) FROM livetv_programmes p WHERE p.source_id=g.source_id AND p.channel_id=g.guide_id AND p.end_ms>?1),
		(SELECT MAX(end_ms) FROM livetv_programmes p WHERE p.source_id=g.source_id AND p.channel_id=g.guide_id)
		FROM gc g
		WHERE (?2='' OR g.source_id=?2) AND (?3='' OR `+store.Contains(d, "lower(g.name)", "?3")+` OR `+store.Contains(d, "lower(g.guide_id)", "?3")+` OR `+store.Contains(d, "lower(g.names)", "?3")+`)
		ORDER BY (lower(g.guide_id)=?3 OR lower(g.name)=?3) DESC, lower(g.name), g.source_id, g.guide_id LIMIT ?4`, now, source, q, limit)
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
	cs, err := s.Channels(ctx)
	if err != nil {
		return nil, err
	}
	from := s.o.Now()
	until := from.Add(coverageWindow)
	stats, err := s.guideStats(ctx, from, until)
	if err != nil {
		return nil, err
	}
	out := make([]GuideCoverage, 0, len(cs))
	for _, c := range cs {
		v := GuideCoverage{ChannelID: c.ID}
		if c.Kind == "virtual" {
			ps := Schedule(c, from, until)
			v.Programmes = len(ps)
			if len(ps) > 0 {
				end := ps[len(ps)-1].End.UTC()
				v.Until = &end
			}
		} else if st, ok := stats[guideKey{guideSource(c), c.GuideID}]; ok && c.GuideID != "" {
			v.Programmes = st.window
			v.Until = msTime(st.until)
		}
		out = append(out, v)
	}
	return out, nil
}

var (
	guideCountryPrefix = regexp.MustCompile(`^[\s|]*[\p{L}]{2,3}\s*[:|]\s*`)
	guideBracketed     = regexp.MustCompile(`[\[(]([^\])]*)[\])]`)
	guideQuality       = map[string]bool{"hd": true, "fhd": true, "uhd": true, "4k": true, "8k": true, "sd": true, "hq": true, "lq": true, "hevc": true, "h264": true, "h265": true, "1080p": true, "1080i": true, "720p": true, "2160p": true, "50fps": true, "60fps": true}
)

// normalizeGuideName folds a channel name for matching: case, punctuation,
// country prefixes ("US:", "UK |"), bracketed tags and quality suffixes are
// ignored, while "+1" style timeshifts stay distinct.
func normalizeGuideName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = guideCountryPrefix.ReplaceAllString(s, "")
	s = guideBracketed.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(m[1 : len(m)-1])
		if guideQuality[inner] || len([]rune(inner)) <= 3 && !strings.Contains(inner, "+") {
			return " "
		}
		return " " + inner + " "
	})
	tokens := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' })
	var b strings.Builder
	for _, t := range tokens {
		if guideQuality[t] {
			continue
		}
		b.WriteString(t)
	}
	return b.String()
}

type guideCandidate struct {
	source, guide, name string
	window              int
	until               int64
}

// MatchGuides maps IPTV channels to guide channels from any source: an exact
// tvg-id wins, then a case-insensitive id, then a normalized display name.
// Only guide channels with upcoming programmes are considered, and disabled
// channels only when listed in ChannelIDs. Without
// Overwrite, channels that already have programmes in the next 24 hours are
// left alone.
func (s *Service) MatchGuides(ctx context.Context, req GuideMatchRequest) (GuideMatchResult, error) {
	res := GuideMatchResult{DryRun: req.DryRun, Changes: []GuideMatchChange{}}
	cs, err := s.Channels(ctx)
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
		st, ok := stats[guideKey{src, id}]
		if !ok {
			continue // no upcoming programmes; mapping to it would not help
		}
		cand := &guideCandidate{source: src, guide: id, name: name, window: st.window, until: st.until}
		byID[id] = append(byID[id], cand)
		fold := strings.ToLower(id)
		byFold[fold] = append(byFold[fold], cand)
		var all []string
		json.Unmarshal([]byte(raw), &all)
		seen := map[string]bool{}
		for _, n := range append(all, name) {
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
	want := map[string]bool{}
	for _, id := range req.ChannelIDs {
		want[id] = true
	}
	var changed []Channel
	for _, c := range cs {
		// Off-air channels are only matched when asked for by id; otherwise a
		// lineup with retired channels floods the proposals.
		if c.Kind != "iptv" || len(want) > 0 && !want[c.ID] || len(want) == 0 && !c.Enabled {
			continue
		}
		if !req.Overwrite && c.GuideID != "" && stats[guideKey{guideSource(c), c.GuideID}].window > 0 {
			res.Skipped++
			continue
		}
		var pick *guideCandidate
		method := "id"
		if c.GuideID != "" {
			pick = bestCandidate(c, byID[c.GuideID])
			if pick == nil {
				pick = bestCandidate(c, byFold[strings.ToLower(c.GuideID)])
			}
		}
		if pick == nil {
			method = "name"
			if k := normalizeGuideName(c.Name); k != "" {
				pick = bestCandidate(c, byName[k])
			}
		}
		if pick == nil {
			res.Unmatched++
			continue
		}
		res.Matched++
		if pick.source == guideSource(c) && pick.guide == c.GuideID {
			continue
		}
		res.Changes = append(res.Changes, GuideMatchChange{ChannelID: c.ID, ChannelName: c.Name, GuideSourceID: pick.source, GuideSourceName: sourceNames[pick.source], GuideID: pick.guide, GuideName: pick.name, Method: method})
		c.GuideID = pick.guide
		c.GuideSourceID = pick.source
		if pick.source == c.SourceID {
			c.GuideSourceID = ""
		}
		changed = append(changed, c)
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
		if _, err = tx.ExecContext(ctx, `UPDATE livetv_channels SET data=? WHERE id=?`, b, c.ID); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
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
	if src.PlaylistURL == "" && src.GuideURL == "" || src.RefreshHours < 0 {
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
	if horizon != nil && src.GuideURL != "" {
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
			srcs[i].NextRefresh = &next
		}
	}
	return nil
}

// RefreshSource re-imports a stored source. On failure the existing channels
// and guide are kept and the error is recorded on the source.
func (s *Service) RefreshSource(ctx context.Context, id string) (Source, error) {
	src, err := s.storedSourceByID(ctx, id)
	if err != nil {
		return src, err
	}
	v, err := s.ImportSource(ctx, src)
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
		if _, e := s.o.DB.ExecContext(context.WithoutCancel(ctx), q, msg, at, id); e != nil {
			return v, errors.Join(err, e)
		}
	}
	return v, err
}

// refreshDueSources refreshes every source whose automatic refresh is due and
// returns the ids it attempted.
func (s *Service) refreshDueSources(ctx context.Context) []string {
	// Only scheduling fields are read; inline uploads can be large.
	d := store.DialectOf(s.o.DB)
	rows, err := s.o.DB.QueryContext(ctx, `SELECT id, `+store.JSONText(d, "data", "playlistUrl")+`, `+store.JSONText(d, "data", "guideUrl")+`, `+store.JSONInt(d, "data", "refreshHours")+`, `+store.JSONText(d, "data", "updatedAt")+`, `+store.JSONText(d, "data", "lastErrorAt")+`, `+store.JSONInt(d, "data", "refreshFailures")+` FROM livetv_sources ORDER BY id`)
	if err != nil {
		return nil
	}
	var srcs []Source
	for rows.Next() {
		var id string
		var playlist, guide, updated, failed sql.NullString
		var hours, failures sql.NullInt64
		if rows.Scan(&id, &playlist, &guide, &hours, &updated, &failed, &failures) != nil {
			continue
		}
		src := Source{ID: id, PlaylistURL: playlist.String, GuideURL: guide.String, RefreshHours: int(hours.Int64), RefreshFailures: int(failures.Int64)}
		src.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated.String)
		if t, e := time.Parse(time.RFC3339Nano, failed.String); e == nil {
			src.LastErrorAt = &t
		}
		srcs = append(srcs, src)
	}
	rows.Close()
	if rows.Err() != nil || s.planRefresh(ctx, srcs) != nil {
		return nil
	}
	now := s.o.Now()
	var due []string
	for _, src := range srcs {
		if src.NextRefresh != nil && !now.Before(*src.NextRefresh) {
			due = append(due, src.ID)
		}
	}
	for _, id := range due {
		if ctx.Err() != nil {
			break
		}
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		s.RefreshSource(rctx, id)
		cancel()
	}
	return due
}

func (s *Service) refreshLoop() {
	defer s.wg.Done()
	t := time.NewTicker(refreshCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.refreshDueSources(s.ctx)
		}
	}
}
