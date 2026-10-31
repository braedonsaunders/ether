// Package markers supplies release-duration-specific skip ranges. Remote
// lookups are opt-in and cached; no submissions are ever sent.
package markers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("markers", 1, `CREATE TABLE markers_cache(rating_key INTEGER NOT NULL,runtime_ms INTEGER NOT NULL,ranges TEXT NOT NULL,expires_at INTEGER NOT NULL,PRIMARY KEY(rating_key,runtime_ms));`)
}

type Marker struct {
	Type    string `json:"type"`
	StartMS int64  `json:"startTimeOffset"`
	EndMS   int64  `json:"endTimeOffset"`
	Final   bool   `json:"final"`
	Source  string `json:"source"`
}
type Options struct {
	Config func() (bool, string) // current TheIntroDB enabled state and optional end-user key

	Logger     *slog.Logger
	DB         *sql.DB
	Enabled    bool
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	TTL        time.Duration
	Now        func() time.Time

	// Aligner, when set, shifts TheIntroDB markers from the reference
	// runtime they were submitted against into the played release's
	// timeline (dialogue-based, no video downloaded).
	Aligner core.TimelineAligner
	// AlignTimeout bounds one marker-shifting alignment (default 10s);
	// on failure the unshifted markers are kept.
	AlignTimeout time.Duration
	// Dialogue supplies already-available subtitle cue times for the
	// played release (cached sidecars), used for stinger detection.
	Dialogue func(ctx context.Context, key core.RatingKey, info core.MediaInfo) ([]DialogueCue, bool)
}
type Service struct {
	o Options
}

func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://api.theintrodb.org/v4"
	}
	if o.HTTPClient == nil {
		o.HTTPClient = egress.Client(egress.CapMetadata, 2*time.Second)
	}
	if o.TTL <= 0 {
		o.TTL = 7 * 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}
}
func valid(m Marker, d time.Duration) bool {
	return (m.Type == "intro" || m.Type == "credits" || m.Type == "recap") && m.StartMS >= 0 && m.EndMS > m.StartMS && m.EndMS <= d.Milliseconds()
}

// chapterSlack is how far a chapter may end past the media duration and
// still be read as ending at it: container durations and the last
// chapter's end are probed separately and routinely disagree by a few
// milliseconds.
const chapterSlack = 2 * time.Second

// chapterEnd is a chapter's end, snapped to the duration when it overruns
// it by no more than chapterSlack.
func chapterEnd(c core.Chapter, d time.Duration) time.Duration {
	if d > 0 && c.End > d && c.End-d <= chapterSlack {
		return d
	}
	return c.End
}

// FromChapters uses explicit chapter labels only; generic Chapter 01 is not evidence.
func FromChapters(info core.MediaInfo) []Marker {
	var out []Marker
	for _, c := range info.Chapters {
		c.End = chapterEnd(c, info.Duration)
		label := strings.ToLower(strings.TrimSpace(c.Title))
		kind := ""
		switch label {
		case "intro", "opening", "opening credits", "opening titles":
			kind = "intro"
		case "credits", "end credits", "closing credits", "ending":
			kind = "credits"
		case "recap", "previously on":
			kind = "recap"
		}
		m := Marker{Type: kind, StartMS: c.Start.Milliseconds(), EndMS: c.End.Milliseconds(), Final: kind == "credits" && c.End >= info.Duration-time.Second, Source: "chapter"}
		if valid(m, info.Duration) {
			out = append(out, m)
		}
	}
	return out
}
func (s *Service) cached(ctx context.Context, key core.RatingKey, d time.Duration) ([]Marker, bool, error) {
	if s.o.DB == nil {
		return nil, false, nil
	}
	var raw string
	var exp int64
	err := s.o.DB.QueryRowContext(ctx, `SELECT ranges,expires_at FROM markers_cache WHERE rating_key=? AND runtime_ms=?`, int64(key), d.Milliseconds()).Scan(&raw, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if exp <= s.o.Now().Unix() {
		return nil, false, nil
	}
	var out []Marker
	err = json.Unmarshal([]byte(raw), &out)
	return out, err == nil, err
}
func (s *Service) Put(ctx context.Context, key core.RatingKey, d time.Duration, ranges []Marker) error {
	if s.o.DB == nil {
		return errors.New("marker cache requires database")
	}
	for _, m := range ranges {
		if !valid(m, d) {
			return errors.New("invalid marker range")
		}
	}
	b, err := json.Marshal(ranges)
	if err != nil {
		return err
	}
	_, err = s.o.DB.ExecContext(ctx, `INSERT INTO markers_cache VALUES(?,?,?,?) ON CONFLICT(rating_key,runtime_ms) DO UPDATE SET ranges=excluded.ranges,expires_at=excluded.expires_at`, int64(key), d.Milliseconds(), string(b), s.o.Now().Add(s.o.TTL).Unix())
	return err
}

// Markers returns the skip ranges for one probed release: chapter and
// remote markers (shifted into this release's timeline when they were
// submitted against a different cut), with stinger scenes cut out of the
// credits ranges.
func (s *Service) Markers(ctx context.Context, key core.RatingKey, info core.MediaInfo) ([]Marker, error) {
	out, err := s.rawMarkers(ctx, key, info)
	return s.applyStingers(ctx, key, info, out), err
}

// CachedMarkers uses chapters and locally cached ranges only. It never fetches
// or aligns remote markers, making it safe for non-resolving detail pages.
func (s *Service) CachedMarkers(ctx context.Context, key core.RatingKey, info core.MediaInfo) ([]Marker, error) {
	if info.Estimated || info.Duration <= 0 {
		return nil, nil
	}
	out := FromChapters(info)
	cached, ok, err := s.cached(ctx, key, info.Duration)
	if err == nil && ok {
		out = merge(out, cached)
	}
	return s.applyStingers(ctx, key, info, out), err
}

// applyStingers detects mid-/post-credits scenes (chapters first, then
// resumed dialogue from already-available subtitle text) and cuts them
// out of the credits markers.
func (s *Service) applyStingers(ctx context.Context, key core.RatingKey, info core.MediaInfo, out []Marker) []Marker {
	if len(out) == 0 || info.Duration <= 0 {
		return out
	}
	stingers := StingersFromChapters(info)
	if s.o.Dialogue != nil {
		creditsStart, hasCredits := time.Duration(0), false
		for _, m := range out {
			if m.Type == "credits" && (!hasCredits || time.Duration(m.StartMS)*time.Millisecond < creditsStart) {
				creditsStart, hasCredits = time.Duration(m.StartMS)*time.Millisecond, true
			}
		}
		if hasCredits {
			if cues, ok := s.o.Dialogue(ctx, key, info); ok {
				stingers = append(stingers, StingersFromDialogue(cues, creditsStart, info.Duration)...)
			}
		}
	}
	return ApplyStingers(out, stingers, info.Duration)
}

func (s *Service) rawMarkers(ctx context.Context, key core.RatingKey, info core.MediaInfo) ([]Marker, error) {
	if info.Estimated {
		return nil, nil
	}
	out := FromChapters(info)
	cached, ok, err := s.cached(ctx, key, info.Duration)
	if err != nil {
		return out, err
	}
	if ok {
		return merge(out, cached), nil
	}
	enabled, apiKey := s.o.Enabled, s.o.APIKey
	if s.o.Config != nil {
		enabled, apiKey = s.o.Config()
	}
	if !enabled || info.Duration <= 0 || (key.Kind() != core.KindEpisode && key.Kind() != core.KindMovie) {
		return out, nil
	}
	remote, refDuration, err := s.fetch(ctx, key, info.Duration, apiKey)
	if err != nil {
		return out, err
	}
	remote = s.shiftToPlayed(ctx, key, remote, refDuration, info.Duration)
	if s.o.DB != nil {
		if err = s.Put(ctx, key, info.Duration, remote); err != nil {
			return merge(out, remote), err
		}
	}
	return merge(out, remote), nil
}
func merge(a, b []Marker) []Marker {
	// Existing sources win for each kind, while retaining every range from
	// the chosen source (TheIntroDB may supply several intros/credit ranges).
	blocked := map[string]bool{}
	for _, m := range a {
		blocked[m.Type] = true
	}
	for _, m := range b {
		if !blocked[m.Type] {
			a = append(a, m)
		}
	}
	return a
}

// fetch reads TheIntroDB markers. The second result is the reference
// runtime the returned timestamps were submitted against
// (group_duration_ms, present with details=true), or 0 when the API does
// not say; markers are validated against it when known.
func (s *Service) fetch(ctx context.Context, key core.RatingKey, d time.Duration, apiKey string) ([]Marker, time.Duration, error) {
	q := url.Values{"tmdb_id": {strconv.FormatInt(key.TMDB(), 10)}, "duration_ms": {strconv.FormatInt(d.Milliseconds(), 10)}, "merge_unknown": {"false"}, "details": {"true"}}
	if key.Kind() == core.KindEpisode {
		q.Set("season", strconv.Itoa(key.Season()))
		q.Set("episode", strconv.Itoa(key.Episode()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.o.BaseURL, "/")+"/media?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Ether/1")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := s.o.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, 0, nil
	}
	if resp.StatusCode != 200 {
		return nil, 0, fmt.Errorf("theintrodb: HTTP %d", resp.StatusCode)
	}
	var body map[string]json.RawMessage
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, 0, err
	}
	ref := d
	for _, field := range []string{"group_duration_ms", "groupDurationMs"} {
		var ms *int64
		if raw := body[field]; len(raw) > 0 && json.Unmarshal(raw, &ms) == nil && ms != nil && *ms > 0 {
			ref = time.Duration(*ms) * time.Millisecond
			break
		}
	}
	var out []Marker
	for _, kind := range []string{"intro", "credits", "recap"} {
		raw := body[kind]
		if len(raw) == 0 {
			continue
		}
		var ranges []struct {
			Start *int64 `json:"start_ms"`
			End   *int64 `json:"end_ms"`
		}
		if err = json.Unmarshal(raw, &ranges); err != nil {
			return nil, 0, err
		}
		for _, r := range ranges {
			start, end := int64(0), ref.Milliseconds()
			if r.Start != nil {
				start = *r.Start
			}
			if r.End != nil {
				end = *r.End
			}
			m := Marker{Type: kind, StartMS: start, EndMS: end, Final: kind == "credits" && end == ref.Milliseconds(), Source: "theintrodb"}
			if valid(m, ref) {
				out = append(out, m)
			}
		}
	}
	if ref == d {
		ref = 0 // no distinct reference runtime reported
	}
	return out, ref, nil
}

// shiftToPlayed maps remote markers from their reference runtime into the
// played release's timeline via dialogue alignment. When the runtimes
// agree, no aligner is configured, or alignment is not confident, the
// markers are kept as-is (validated against the played runtime, exactly
// the previous behaviour).
func (s *Service) shiftToPlayed(ctx context.Context, key core.RatingKey, remote []Marker, ref, played time.Duration) []Marker {
	clamp := func(in []Marker) []Marker {
		out := in[:0]
		for _, m := range in {
			// A marker that runs to the reference tail (an open-ended
			// credits range) runs to ours: unshifted, it would end past a
			// shorter played release and be dropped, or stop short of a
			// longer one's end and lose final.
			if ref > 0 && m.EndMS >= ref.Milliseconds()-500 && m.StartMS < played.Milliseconds() {
				m.EndMS = played.Milliseconds()
				m.Final = m.Type == "credits"
			}
			if valid(m, played) {
				out = append(out, m)
			}
		}
		return out
	}
	if len(remote) == 0 || ref <= 0 || s.o.Aligner == nil {
		return clamp(remote)
	}
	if diff := ref - played; diff < 1500*time.Millisecond && diff > -1500*time.Millisecond {
		return clamp(remote)
	}
	timeout := s.o.AlignTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req := core.AlignRequest{Key: key, FromDuration: ref, ToDuration: played}
	shifted := make([]Marker, 0, len(remote))
	for _, m := range remote {
		start, ok := s.o.Aligner.AlignTime(ctx, req, time.Duration(m.StartMS)*time.Millisecond)
		if !ok {
			return clamp(remote)
		}
		end := played // a marker running to the reference tail runs to ours
		if m.EndMS < ref.Milliseconds()-500 {
			mapped, ok := s.o.Aligner.AlignTime(ctx, req, time.Duration(m.EndMS)*time.Millisecond)
			if !ok {
				return clamp(remote)
			}
			end = mapped
		}
		nm := Marker{Type: m.Type, StartMS: start.Milliseconds(), EndMS: end.Milliseconds(),
			Final: m.Type == "credits" && end >= played-1500*time.Millisecond, Source: m.Source + "+aligned"}
		if valid(nm, played) {
			shifted = append(shifted, nm)
		}
	}
	if len(shifted) == 0 {
		return clamp(remote)
	}
	return shifted
}
