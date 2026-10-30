package resolver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// The release console is the owner's view into release selection: an
// interactive search that explains every result with the same ranking
// Resolve uses, owner pins (pins.go), the blocklist (blocklist.go) and the
// persisted resolution history (history.go).
//
// All SQL here is written to run unchanged on PostgreSQL as well as SQLite.
func init() {
	store.Register("resolver", 3, `
ALTER TABLE resolver_bad ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE resolver_bad ADD COLUMN indexer TEXT NOT NULL DEFAULT '';
ALTER TABLE resolver_bad ADD COLUMN rating_key INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resolver_bad ADD COLUMN title_name TEXT NOT NULL DEFAULT '';
ALTER TABLE resolver_bad ADD COLUMN source TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE resolver_bad ADD COLUMN manual INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resolver_bad ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE resolver_bad SET updated_at = created_at;
CREATE INDEX resolver_bad_updated ON resolver_bad(updated_at);
CREATE INDEX resolver_bad_title ON resolver_bad(rating_key);
CREATE TABLE resolver_pins (
  rating_key INTEGER PRIMARY KEY,
  release_id TEXT NOT NULL,
  release_json TEXT NOT NULL,
  title_name TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  pinned_at INTEGER NOT NULL,
  last_status TEXT NOT NULL DEFAULT '',
  last_reason TEXT NOT NULL DEFAULT '',
  last_checked_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE resolver_history (
  id INTEGER PRIMARY KEY,
  resolved_at INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  title_name TEXT NOT NULL DEFAULT '',
  profile TEXT NOT NULL DEFAULT '',
  release_id TEXT NOT NULL DEFAULT '',
  release_name TEXT NOT NULL DEFAULT '',
  indexer TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  failover INTEGER NOT NULL DEFAULT 0,
  pinned INTEGER NOT NULL DEFAULT 0,
  candidates INTEGER NOT NULL DEFAULT 0,
  trace TEXT NOT NULL DEFAULT ''
);
CREATE INDEX resolver_history_at ON resolver_history(resolved_at);
CREATE INDEX resolver_history_title ON resolver_history(rating_key, resolved_at);`)
}

const (
	maxRememberedReleases = 4096
	maxHealthResults      = 1024
	defaultHistoryMax     = 5000
	defaultHistoryMaxAge  = 60 * 24 * time.Hour
	maxTraceCandidates    = 50
)

// Console errors the owner API maps to client errors.
var (
	ErrUnknownRelease = errors.New("release is not among recent search results; search again")
	ErrUnknownProfile = errors.New("no such quality profile")
	ErrPinBlocked     = errors.New("release is blocklisted; unblock it before pinning")
	ErrNotPinnable    = errors.New("only movies and episodes can be pinned")
)

// Special console profile names.
const (
	// ProfileNone lists results without profile gates: identity, client
	// and blocklist rules still apply, quality policy does not.
	ProfileNone = "none"
	// ProfileDownload is the owner's default profile reshaped for an
	// offline download, as SelectProfile does for caps.Download.
	ProfileDownload = "download"
)

type rememberedRelease struct {
	rel       core.Release
	titleKey  core.RatingKey
	titleName string
}

// consoleState holds the console's in-memory state. Pins are mirrored
// from the database; remembered releases fill blocklist metadata and let
// the owner act on a search result by id.
type consoleState struct {
	mu         sync.Mutex
	pins       map[core.RatingKey]Pin
	recent     map[string]rememberedRelease
	order      []string
	checks     map[string]HealthResult
	blocks     map[string]BlockEntry // memory-only mode (no database)
	history    []HistoryRecord       // memory-only mode (no database)
	historySeq int64
	historyMax int
	historyAge time.Duration
}

func (c *consoleState) init(opts Options) {
	c.pins = map[core.RatingKey]Pin{}
	c.recent = map[string]rememberedRelease{}
	c.checks = map[string]HealthResult{}
	c.blocks = map[string]BlockEntry{}
	c.historyMax = opts.HistoryMax
	if c.historyMax <= 0 {
		c.historyMax = defaultHistoryMax
	}
	c.historyAge = opts.HistoryMaxAge
	if c.historyAge <= 0 {
		c.historyAge = defaultHistoryMaxAge
	}
}

// rememberReleases records search results for a title so later owner
// actions and automatic blocklist marks can name the release and title.
func (s *Service) rememberReleases(t *core.Title, results []core.Release) {
	if len(results) == 0 {
		return
	}
	name := ""
	var key core.RatingKey
	if t != nil {
		name, key = consoleTitleName(t), t.Key
	}
	c := &s.con
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rel := range results {
		if rel.ID == "" {
			continue
		}
		if _, ok := c.recent[rel.ID]; !ok {
			c.order = append(c.order, rel.ID)
		}
		c.recent[rel.ID] = rememberedRelease{rel: rel, titleKey: key, titleName: name}
	}
	if over := len(c.order) - maxRememberedReleases; over > 0 {
		for _, id := range c.order[:over] {
			delete(c.recent, id)
		}
		c.order = append([]string(nil), c.order[over:]...)
	}
}

// LookupRelease finds a release by id among recent search results and
// owner pins.
func (s *Service) LookupRelease(id string) (core.Release, bool) {
	c := &s.con
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.recent[id]; ok {
		return r.rel, true
	}
	for _, p := range c.pins {
		if p.Release.ID == id {
			return p.Release, true
		}
	}
	return core.Release{}, false
}

// consoleTitleName is the owner-facing name of a title: "Show S01E02" for
// episodes, the title otherwise.
func consoleTitleName(t *core.Title) string {
	if t == nil {
		return ""
	}
	if t.Type == core.TypeEpisode || t.Key.Kind() == core.KindEpisode {
		show := t.ShowTitle
		if show == "" {
			show = t.Title
		}
		season, episode := t.Season, t.Episode
		if season == 0 && episode == 0 {
			season, episode = t.Key.Season(), t.Key.Episode()
		}
		return fmt.Sprintf("%s S%02dE%02d", show, season, episode)
	}
	if t.Year > 0 {
		return fmt.Sprintf("%s (%d)", t.Title, t.Year)
	}
	return t.Title
}

// ---------------------------------------------------------------------------
// Profiles

// ConsoleProfiles lists the profiles interactive search can rank with:
// the configured profiles, plus ProfileNone and ProfileDownload.
func (s *Service) ConsoleProfiles() (def string, names []string) {
	q := s.qualityCfg()
	for _, p := range q.Profiles {
		names = append(names, p.Name)
	}
	def = q.Default
	if _, ok := profileByName(q.Profiles, def); !ok {
		def = ""
		if len(q.Profiles) > 0 {
			def = q.Profiles[0].Name
		}
	}
	return def, names
}

// consoleProfile resolves an interactive-search profile name. "" is the
// owner's default profile. Profiles are not clamped to any client: the
// console ranks as a capable client would; client gates apply at play.
func (s *Service) consoleProfile(name string) (core.QualityProfile, core.ClientCaps, error) {
	q := s.qualityCfg()
	switch name {
	case ProfileNone:
		return core.QualityProfile{Name: ProfileNone, AllowHDR: true, AllowDolbyVision: true, AllowCam: true}, core.ClientCaps{}, nil
	case ProfileDownload:
		base := SelectProfile(q, core.ClientCaps{}, nil)
		if p, ok := profileByName(q.Profiles, q.Default); ok {
			base = p
		}
		caps := core.ClientCaps{Download: true}
		// This path bypasses SelectProfile, so snapshot the configured
		// ceiling here too, before the download reshape lowers it.
		return downloadProfile(snapshotConfiguredMaxHeight(copyProfile(base)), caps), caps, nil
	case "":
		if p, ok := profileByName(q.Profiles, q.Default); ok {
			return copyProfile(p), core.ClientCaps{}, nil
		}
		if len(q.Profiles) > 0 {
			return copyProfile(q.Profiles[0]), core.ClientCaps{}, nil
		}
		return defaultProfile(), core.ClientCaps{}, nil
	}
	if p, ok := profileByName(q.Profiles, name); ok {
		return copyProfile(p), core.ClientCaps{}, nil
	}
	return core.QualityProfile{}, core.ClientCaps{}, fmt.Errorf("%w: %q", ErrUnknownProfile, name)
}

func profileByName(ps []core.QualityProfile, name string) (core.QualityProfile, bool) {
	if name == "" {
		return core.QualityProfile{}, false
	}
	for _, p := range ps {
		if p.Name == name {
			return p, true
		}
	}
	return core.QualityProfile{}, false
}

func copyProfile(p core.QualityProfile) core.QualityProfile {
	p.PreferredSources = append([]core.Source(nil), p.PreferredSources...)
	p.PreferredCodecs = append([]string(nil), p.PreferredCodecs...)
	p.PreferredAudio = append([]string(nil), p.PreferredAudio...)
	p.PreferredLanguages = append([]string(nil), p.PreferredLanguages...)
	p.AllowedQualities = append([]string(nil), p.AllowedQualities...)
	if p.CustomFormatScores != nil {
		m := make(map[string]int, len(p.CustomFormatScores))
		for k, v := range p.CustomFormatScores {
			m[k] = v
		}
		p.CustomFormatScores = m
	}
	return p
}

// ---------------------------------------------------------------------------
// Interactive search

// SearchOptions tune an interactive search.
type SearchOptions struct {
	// Profile names the quality profile to rank with: "" for the owner's
	// default, ProfileNone, ProfileDownload, or a configured profile.
	Profile string
	// Fresh bypasses the search cache (the fresh answer is cached).
	Fresh bool
}

// browserRelease is rel as the owner's browser may see it. NZB links carry
// the indexer's API key, magnet links can carry tracker passkeys, and an
// addon's stream URL and headers can carry the addon's (or its debrid
// account's) credentials; the console never needs any of them (pins and
// health checks look releases up by ID in the server's own results), so
// all are dropped. StreamOrigin is the owner's own addon origin and stays.
func browserRelease(rel core.Release) core.Release {
	rel.NZBURL, rel.MagnetURL = "", ""
	rel.StreamURL, rel.StreamHeaders = "", nil
	return rel
}

// browserPin is p with its release made safe for the browser.
func browserPin(p Pin) Pin {
	p.Release = browserRelease(p.Release)
	return p
}

// ConsoleRelease is one search result as Resolve would judge it.
type ConsoleRelease struct {
	Release       core.Release  `json:"release"`
	Score         float64       `json:"score"`
	FormatScore   int           `json:"formatScore"`
	CustomFormats []FormatMatch `json:"customFormats,omitempty"`
	Compatibility string        `json:"compatibility,omitempty"`
	// Rejected and Reason are Resolve's verdict for this profile.
	Rejected bool   `json:"rejected"`
	Reason   string `json:"reason,omitempty"`
	// Rank is the 1-based position among accepted releases in the order
	// Resolve tries them (the pin first); 0 when rejected.
	Rank        int           `json:"rank,omitempty"`
	Winner      bool          `json:"winner,omitempty"`
	Blocked     bool          `json:"blocked,omitempty"`
	BlockReason string        `json:"blockReason,omitempty"`
	Pinned      bool          `json:"pinned,omitempty"`
	Health      *HealthResult `json:"health,omitempty"`
	// Route explains a move by source routing (routing.go).
	Route string `json:"route,omitempty"`
}

// ConsoleSearch is an interactive search answer.
type ConsoleSearch struct {
	TitleKey   core.RatingKey   `json:"titleKey,string"`
	TitleName  string           `json:"titleName"`
	Profile    string           `json:"profile"`
	Cached     bool             `json:"cached"`
	SearchedAt time.Time        `json:"searchedAt"`
	Duration   time.Duration    `json:"duration"`
	Winner     string           `json:"winner,omitempty"`  // release Resolve would try first
	Routing    string           `json:"routing,omitempty"` // source-routing decision, if any
	Pin        *Pin             `json:"pin,omitempty"`
	PinNote    string           `json:"pinNote,omitempty"`
	Releases   []ConsoleRelease `json:"releases"`
}

// SearchReleases runs the indexer search for a title (through the search
// cache unless opts.Fresh) and returns every result with the score,
// custom formats and rejection reason Resolve would give it, plus its
// blocklist, pin and last known health. It never health-checks.
func (s *Service) SearchReleases(ctx context.Context, t *core.Title, opts SearchOptions) (*ConsoleSearch, error) {
	if t == nil {
		return nil, fmt.Errorf("resolver: nil title")
	}
	if s.indexer == nil {
		return nil, fmt.Errorf("resolver: no indexer configured")
	}
	profile, caps, err := s.consoleProfile(opts.Profile)
	if err != nil {
		return nil, err
	}
	sreq := searchRequestFor(t)
	start := time.Now()
	results, cached, err := s.searchCached(ctx, sreq, opts.Fresh)
	if err != nil {
		return nil, err
	}
	out := &ConsoleSearch{TitleKey: t.Key, TitleName: consoleTitleName(t), Profile: profile.Name,
		Cached: cached, SearchedAt: s.now(), Duration: time.Since(start), Releases: []ConsoleRelease{}}
	s.rememberReleases(t, results)
	if s.searchObserved != nil {
		s.searchObserved(ctx, t, results)
	}

	req := core.ResolveRequest{Title: t, Client: caps}
	pinID := ""
	if p, ok := s.PinFor(t.Key); ok {
		out.Pin = &p
	}
	pin, note := s.pinForResolve(req)
	out.PinNote = note
	if pin != nil {
		pinID = pin.Release.ID
		results = withRelease(results, pin.Release)
	}
	var trace Trace
	cands, _, err := s.rankCandidates(ctx, req, sreq, profile, results, &trace, pinID)
	if err != nil {
		return nil, err
	}
	cands = pinFirst(cands, pinID)
	rank := map[string]int{}
	for i, c := range cands {
		rank[c.rel.ID] = i + 1
	}
	if len(cands) > 0 {
		out.Winner = cands[0].rel.ID
		// New candidates revoke an old negative verdict; they are not yet
		// health-verified. Let the next Play validate them normally instead
		// of returning the earlier failure for the rest of its cooloff.
		if e, known := s.getAvailability(ctx, int64(t.Key)); known && !e.available {
			if err := s.forgetAvailability(ctx, t.Key); err != nil {
				s.log.Warn("resolver: clearing stale unavailability failed", "title", t.Key, "err", err)
			}
		}
	}
	out.Routing = trace.Routing
	// rankCandidates writes one trace row per result, in result order.
	for i, ct := range trace.Candidates {
		rel := results[i]
		cr := ConsoleRelease{Release: browserRelease(rel), Score: ct.Score, CustomFormats: ct.CustomFormats,
			Compatibility: ct.CompatibilityReason, Rejected: ct.Rejected, Reason: ct.Reason,
			Rank: rank[rel.ID], Winner: rel.ID == out.Winner && !ct.Rejected, Pinned: ct.Pinned, Route: ct.Route}
		// Rejected releases are scored too, so the owner can compare.
		if sc, err := s.scoreRelease(rel, profile, caps, t); err == nil {
			cr.FormatScore = sc.format
			if ct.Rejected {
				cr.Score, cr.CustomFormats = sc.total, sc.matches
			}
		}
		cr.Release.Score = cr.Score
		reason, bad := s.badReason(rel.ID)
		cr.Blocked, cr.BlockReason = bad, reason
		if h, ok := s.lastHealth(ctx, rel.ID); ok {
			cr.Health = &h
		}
		out.Releases = append(out.Releases, cr)
	}
	sort.SliceStable(out.Releases, func(i, j int) bool {
		a, b := out.Releases[i], out.Releases[j]
		if (a.Rank > 0) != (b.Rank > 0) {
			return a.Rank > 0
		}
		if a.Rank > 0 {
			return a.Rank < b.Rank
		}
		return a.Score > b.Score
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// Health

// HealthResult is the last known health of a release.
type HealthResult struct {
	ReleaseID string        `json:"releaseId"`
	CheckedAt time.Time     `json:"checkedAt"`
	Healthy   bool          `json:"healthy"`
	Reason    string        `json:"reason,omitempty"`
	Duration  time.Duration `json:"duration,omitempty"`
	// Source is "check" (an owner check), "verified" (passed a check
	// during resolution, still trusted) or "blocked" (on the blocklist).
	Source string `json:"source"`
	// Blocked reports the check put the release on the blocklist.
	Blocked bool `json:"blocked,omitempty"`
}

// lastHealth reports what is known about a release's health without
// checking it.
func (s *Service) lastHealth(ctx context.Context, id string) (HealthResult, bool) {
	s.con.mu.Lock()
	h, ok := s.con.checks[id]
	s.con.mu.Unlock()
	if ok {
		return h, true
	}
	reason, bad := s.badReason(id)
	if bad {
		return HealthResult{ReleaseID: id, Reason: reason, Source: "blocked"}, true
	}
	if at, ok := s.verifiedAt(ctx, id); ok {
		return HealthResult{ReleaseID: id, CheckedAt: at, Healthy: true, Source: "verified"}, true
	}
	return HealthResult{}, false
}

// verifiedAt is the time of a release's last passed health check while it
// is still trusted (healthTTL).
func (s *Service) verifiedAt(ctx context.Context, id string) (time.Time, bool) {
	if !s.recentlyVerified(ctx, id) {
		return time.Time{}, false
	}
	s.mu.RLock()
	at, ok := s.verified[id]
	s.mu.RUnlock()
	return at, ok
}

// CheckRelease health-checks one release on demand (a fresh check, never
// the verified cache): FetchNZB plus the health checker, exactly as
// resolution validates a candidate. A pass refreshes the verified cache;
// a definitive failure (missing or corrupt articles, a rejected release)
// is blocklisted automatically like a failed optimistic check. Transient
// failures are reported but change nothing. titleKey, when known, names
// the title for the blocklist.
func (s *Service) CheckRelease(ctx context.Context, releaseID string, titleKey core.RatingKey) (HealthResult, error) {
	rel, ok := s.LookupRelease(releaseID)
	if !ok {
		return HealthResult{}, ErrUnknownRelease
	}
	if s.indexer == nil {
		return HealthResult{}, fmt.Errorf("resolver: no indexer configured")
	}
	start := time.Now()
	res := HealthResult{ReleaseID: releaseID, Source: "check"}
	definitive := false
	nzb, err := s.indexer.FetchNZB(ctx, rel)
	switch {
	case err != nil:
		res.Reason = "nzb fetch: " + err.Error()
		definitive = permanentFailure(err)
	case s.health == nil:
		res.Healthy = true
		res.Reason = "no health checker configured"
	default:
		healthy, herr := core.CheckReleaseHealth(ctx, s.health, releaseID, nzb)
		switch {
		case healthy && herr == nil:
			res.Healthy = true
			s.markVerified(ctx, releaseID)
		case herr == nil:
			res.Reason, definitive = "health: unhealthy", true
		default:
			res.Reason, definitive = herr.Error(), permanentFailure(herr)
		}
	}
	if ctx.Err() != nil {
		return HealthResult{}, ctx.Err()
	}
	res.CheckedAt, res.Duration = s.now(), time.Since(start)
	if res.Healthy || definitive {
		s.recordReputation(ctx, rel.Indexer, res.Healthy)
	}
	if definitive {
		s.rememberTitleFor(rel, titleKey)
		if err := s.MarkBad(ctx, releaseID, "owner health check: "+res.Reason); err != nil {
			return HealthResult{}, err
		}
		res.Blocked = true
	}
	s.con.mu.Lock()
	if len(s.con.checks) >= maxHealthResults {
		oldest := ""
		var at time.Time
		for id, h := range s.con.checks {
			if oldest == "" || h.CheckedAt.Before(at) {
				oldest, at = id, h.CheckedAt
			}
		}
		delete(s.con.checks, oldest)
	}
	s.con.checks[releaseID] = res
	pinned := []core.RatingKey{}
	for k, p := range s.con.pins {
		if p.Release.ID == releaseID {
			pinned = append(pinned, k)
		}
	}
	s.con.mu.Unlock()
	status, reason := PinHealthy, ""
	switch {
	case definitive:
		status, reason = PinUnhealthy, res.Reason
	case !res.Healthy:
		status, reason = PinUnavailable, res.Reason
	}
	for _, k := range pinned {
		s.notePin(ctx, k, releaseID, status, reason)
	}
	return res, nil
}

// rememberTitleFor attaches a title to a remembered release that has none.
func (s *Service) rememberTitleFor(rel core.Release, key core.RatingKey) {
	if key == 0 {
		return
	}
	s.con.mu.Lock()
	defer s.con.mu.Unlock()
	if r, ok := s.con.recent[rel.ID]; ok && r.titleKey == 0 {
		r.titleKey = key
		s.con.recent[rel.ID] = r
	}
}

// ---------------------------------------------------------------------------
// Forget

// Forget clears what the resolver remembers about a title's search: its
// availability answer (and cool-off) and its cached search results, so
// the next play or search asks the indexers afresh.
func (s *Service) Forget(ctx context.Context, t *core.Title) error {
	if t == nil {
		return fmt.Errorf("resolver: nil title")
	}
	key := searchCacheKey(searchRequestFor(t))
	s.mu.Lock()
	delete(s.searchCache, key)
	delete(s.avail, int64(t.Key))
	s.mu.Unlock()
	return s.forgetAvailability(ctx, t.Key)
}

func (s *Service) forgetAvailability(ctx context.Context, key core.RatingKey) error {
	s.mu.Lock()
	delete(s.avail, int64(key))
	s.mu.Unlock()
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM resolver_availability WHERE rating_key=?`, int64(key)); err != nil {
			return err
		}
	}
	s.notify("avail:" + key.String())
	return nil
}

// likePattern escapes a substring for LIKE ... ESCAPE '\' and lowercases
// it; callers compare against LOWER(column) (LIKE's case sensitivity
// differs between SQLite and PostgreSQL).
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(strings.ToLower(q)) + "%"
}
