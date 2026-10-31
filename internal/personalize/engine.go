// Package personalize is Ether's Netflix-style personalization engine.
//
// Viewers only ever use Plex. The engine makes each viewer's Plex Home and
// library sections feel hand-curated for them, invisibly, by choosing
// which rows they get, in which order, with which titles, for the device
// and time they are watching:
//
//   - a taste model per viewer (taste.go): affinities for genres,
//     keywords, people, decades and languages, with time decay, learned
//     from completions, rewatches, abandons, ratings and impressions that
//     never led to a play, plus habits (session length, peak hours,
//     device, binge and rewatch tendencies) and owner overrides;
//   - candidate rows from generators (generators.go): Continue Watching,
//     Up Next, Top 10, Because you watched, More from <person>, Trending in
//     your taste, Fans of what you watch also loved and Trending with fans
//     of what you watch (crowd.go, buzz.go), Hidden gems, New this week,
//     Comfort rewatches, Something different, Short on time, Finish the
//     franchise, owner collections, LLM themes, Trakt, and popularity rows
//     for new viewers;
//   - assembly per request context (assemble.go): context-aware item
//     scoring, MMR diversity, cross-row dedupe, bandit row ranking with
//     daily-seeded Thompson exploration (bandit.go), the owner's Home
//     layout document (homelayout.go) and daily rotation;
//   - a feedback loop: every served row is recorded as an impression
//     (impressions.go) and every play is attributed to the row it came
//     from (plays.go), which feeds both the taste model and the bandit.
//
// Models are built in the background (on a schedule, after watch-state
// changes and on demand) and cached per viewer; a /hubs request only runs
// assembly over the cache and never waits on TMDB or the LLM.
//
// The Engine implements core.Recommender, core.ContextualRecommender,
// core.ImpressionSink and core.PersonalScorer, plus the owner-facing
// methods the admin API needs (owner.go).
package personalize

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/recommend"
	"github.com/braedonsaunders/ether/internal/state"
)

// UserSource lists the server's users (core.Auth satisfies it).
type UserSource interface {
	Users(ctx context.Context) ([]core.User, error)
}

// Options build an Engine. DB and Catalog are required; every other
// dependency is optional and its rows simply drop out when absent.
// FavoriteSignal is a viewer-owned title or person and its last favorite time.
type FavoriteSignal struct {
	Key      core.RatingKey
	PersonID int64
	AddedAt  time.Time
}

type Options struct {
	// Favorites is optional; nil or empty leaves existing ranking unchanged.
	Favorites func(context.Context, *core.User) ([]FavoriteSignal, error)
	// NotInterested supplies explicit recommendation exclusions for this viewer.
	NotInterested func(context.Context, *core.User) ([]NotInterestedSignal, error)
	// NotInterestedSnapshot returns signals and their atomically-read shared revision.
	NotInterestedSnapshot func(context.Context, *core.User) ([]NotInterestedSignal, uint64, error)
	FilterAvailable       func(context.Context, []core.Title) []core.Title
	// Owner previews use the same effective viewer and title policy as Plex.
	UserPolicy func(*core.User) *core.User
	Allowed    func(*core.User, *core.Title) bool
	DB         *sql.DB
	Catalog    core.Catalog
	State      core.State
	// Events delivers watch-state changes (internal/state.State).
	Events recommend.EventSource
	// Users lists viewers for background precompute (core.Auth).
	Users UserSource
	// Collections supplies owner-designed collection rows.
	Collections core.CollectionRows
	LLM         recommend.LLMSource
	Trakt       recommend.TraktSource
	Playlists   core.Playlists
	// Base answers detail-page Related rows (internal/recommend).
	Base core.Recommender
	// Dependencies constructs consumers of this engine's scorer before any
	// background work starts. This resolves the scorer/row-source construction
	// cycle without publishing a partially wired engine through setters.
	Dependencies func(core.PersonalScorer) (core.Recommender, core.CollectionRows, error)
	// ExtraHomeRows supplies optional integration rows to owner previews.
	ExtraHomeRows func(context.Context, *core.User, core.HubContext, string, int) []core.Hub
	// Sections maps library section ids to their type, for per-user
	// allowed sections (default 1 = movies, 2 = shows).
	Sections map[int]core.MediaType
	// ServerName is the friendly name shown in "Popular on <name>".
	ServerName func() string
	// ImageURL builds browser-loadable artwork URLs for the owner UI
	// (default /ether/api/image?path=…&w=…).
	ImageURL func(path string, width int) string
	Logger   *slog.Logger
	Now      func() time.Time
	Location *time.Location // server local time (default time.Local)
	// Busy reports whether anyone is streaming right now (nil = never).
	// Background work that can wait, like retraining the ranker, waits.
	Busy func() bool
	// IntentEnabled reports whether live session intent (#32) is on
	// (nil = on); consulted per signal so owner edits apply live.
	IntentEnabled func() bool
	// ClockFormat reports the micro-reason finish-time style (#31):
	// "12h", "24h", a locale like "en-GB", or "" for 12h.
	ClockFormat func() string

	ModelTTL        time.Duration // rebuild an active viewer's model after this (default 3h)
	RefreshEvery    time.Duration // background scheduler period (default 15m; <0 disables)
	RebuildDebounce time.Duration // after a watch-state change (default 20s)
	Retention       time.Duration // item impressions (default 60 days)
	Workers         int           // concurrent model builds (default 2)
	ColdWait        time.Duration // how long a request may wait for the first-ever model (default 1.5s)
	OwnerRowsWait   time.Duration // how long a request may wait for owner rows it has never fetched (default 1.5s)
	SourceTimeout   time.Duration // Trakt and owner collection calls (default 16s)
	LLMTimeout      time.Duration // LLM themed rows during a build (default 60s)
	BuildTimeout    time.Duration // one model build (default 3m)
	MaxHomeRows     int           // default 20 across the complete composed page
	MaxSectionRows  int           // default 16
	// MinRowItems is the fewest titles an engine row may show (default 12;
	// negative disables the floor). Continue Watching, Up Next and owner
	// collections are exempt; Top 10 and smaller requested pages use their cap.
	MinRowItems int
	// Exploration scales Thompson sampling (0 = pure exploitation, 1 =
	// full sampling; default 0.5; negative = 0).
	Exploration      float64
	HalfLife         time.Duration // taste decay half-life (default 120 days)
	ImpressionBuffer int           // queued impressions before dropping (default 4096)
	FlushEvery       time.Duration // impression write interval (default 2s)
	Ranker           RankerOptions // learned ranker (ranker.go)
}

func (o *Options) defaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.ImageURL == nil {
		o.ImageURL = func(path string, width int) string {
			return "/ether/api/image?path=" + url.QueryEscape(path) + "&w=" + strconv.Itoa(width)
		}
	}
	if o.ServerName == nil {
		o.ServerName = func() string { return "This Server" }
	}
	if len(o.Sections) == 0 {
		o.Sections = map[int]core.MediaType{1: core.TypeMovie, 2: core.TypeShow}
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.ModelTTL, 3*time.Hour)
	def(&o.RefreshEvery, 15*time.Minute)
	def(&o.RebuildDebounce, 20*time.Second)
	def(&o.Retention, 60*24*time.Hour)
	def(&o.ColdWait, 1500*time.Millisecond)
	def(&o.OwnerRowsWait, 1500*time.Millisecond)
	def(&o.SourceTimeout, 10*time.Second)
	def(&o.LLMTimeout, 60*time.Second)
	def(&o.BuildTimeout, 3*time.Minute)
	def(&o.HalfLife, defaultHalfLife)
	def(&o.FlushEvery, 2*time.Second)
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.MaxHomeRows <= 0 {
		o.MaxHomeRows = 20
	}
	if o.MinRowItems == 0 {
		o.MinRowItems = minRowItems
	}
	if o.MaxSectionRows <= 0 {
		o.MaxSectionRows = 16
	}
	switch {
	case o.Exploration == 0:
		o.Exploration = 0.5
	case o.Exploration < 0:
		o.Exploration = 0
	case o.Exploration > 1:
		o.Exploration = 1
	}
	if o.ImpressionBuffer <= 0 {
		o.ImpressionBuffer = 4096
	}
}

// genericID keys the model served to viewers whose own model is not built
// yet: popularity rows, no personal history.
const genericID int64 = -1

// Engine is the personalization engine.
type Engine struct {
	opts    Options
	log     *slog.Logger
	catalog core.Catalog
	state   core.State
	llm     recommend.LLMSource
	trakt   recommend.TraktSource
	store   *sqlStore
	rec     *recorder
	loc     *time.Location

	mu            sync.Mutex
	closed        bool
	coldDeadline  time.Time // non-zero after the first cold-model wait starts
	models        map[int64]*userModel
	users         map[int64]core.User
	lastCtx       map[int64]core.HubContext
	flights       map[int64]*flight
	debounce      map[int64]*time.Timer
	version       uint64
	homeLayout    core.HomeLayout
	extraHomeRows func(context.Context, *core.User, core.HubContext, string, int) []core.Hub
	layoutVer     uint64
	owner         map[string]*ownerEntry

	memoMu sync.Mutex
	memo   map[memoKey]*memoEntry

	presentationMu sync.Mutex
	presentations  map[memoKey]presentation

	// Netflix-style hooks state (hooks.go), created on first use.
	hookOnce sync.Once
	hook     *hookState

	hubMu    sync.RWMutex
	hubKinds map[string]string

	playMu sync.Mutex
	active map[playKey]*activePlay

	statsMu sync.Mutex
	stats   *statsSnapshot

	invalMu sync.Mutex
	inval   *clusterbus.Invalidator // nil on a standalone server

	actMu  sync.Mutex
	actAt  time.Time
	actRes *activityResult

	rk *rankerState // learned ranker (ranker.go); nil when disabled

	queue  chan int64
	events chan state.Event
	evSync chan chan struct{}
	unsub  func()
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type flight struct {
	done chan struct{}
	// started: a worker is building; again: a request arrived after the
	// build read its inputs, so it must run once more (guarded by e.mu).
	started, again bool
}

var (
	_ core.Recommender           = (*Engine)(nil)
	_ core.ContextualRecommender = (*Engine)(nil)
	_ core.ImpressionSink        = (*Engine)(nil)
	_ core.PersonalScorer        = (*Engine)(nil)
)

// New builds the engine and starts its background work (impression
// writer, event handling, model builders, scheduler). Call Close on
// shutdown.
func New(opts Options) (*Engine, error) {
	if opts.DB == nil {
		return nil, errors.New("personalize: DB is required")
	}
	if opts.Catalog == nil {
		return nil, errors.New("personalize: Catalog is required")
	}
	opts.defaults()
	st := &sqlStore{db: opts.DB, loc: opts.Location}
	e := &Engine{
		opts: opts, log: opts.Logger, catalog: opts.Catalog, state: opts.State, llm: opts.LLM, trakt: opts.Trakt,
		store: st, loc: opts.Location,
		models: map[int64]*userModel{}, users: map[int64]core.User{}, lastCtx: map[int64]core.HubContext{},
		flights: map[int64]*flight{}, debounce: map[int64]*time.Timer{}, owner: map[string]*ownerEntry{},
		hubKinds: map[string]string{}, active: map[playKey]*activePlay{}, memo: map[memoKey]*memoEntry{},
		queue: make(chan int64, 1024), events: make(chan state.Event, 4096), evSync: make(chan chan struct{}),
	}
	e.rec = newRecorder(st, e.log, opts.ImpressionBuffer, opts.FlushEvery)
	var err error
	e.homeLayout, err = st.homeLayout(context.Background())
	if err != nil {
		return nil, err
	}
	if opts.Dependencies != nil {
		e.opts.Base, e.opts.Collections, err = opts.Dependencies(e)
		if err != nil {
			return nil, err
		}
	}
	e.extraHomeRows = opts.ExtraHomeRows
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.startRanker() // learned ranker: load model, serve-log writer, training loop (ranker.go)
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.rec.run(e.ctx) }()
	e.wg.Add(1)
	go e.eventLoop(e.ctx)
	for i := 0; i < opts.Workers; i++ {
		e.wg.Add(1)
		go e.worker(e.ctx)
	}
	if opts.Events != nil {
		e.unsub = opts.Events.Subscribe(e.onEvent)
	}
	e.request(genericID)
	if opts.RefreshEvery > 0 {
		e.wg.Add(1)
		go e.scheduler(e.ctx)
	}
	return e, nil
}

// Close stops background work, writing any queued impressions.
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	for _, t := range e.debounce {
		t.Stop()
	}
	e.mu.Unlock()
	e.SetBus(nil)
	if e.unsub != nil {
		e.unsub()
	}
	e.cancel()
	e.wg.Wait()
}

func (e *Engine) now() time.Time { return e.opts.Now() }

func (e *Engine) serverName() string {
	if n := e.opts.ServerName(); n != "" {
		return n
	}
	return "This Server"
}

// ---- core.Recommender / core.ContextualRecommender ----------------------------

// HomeHubs implements core.Recommender with a context derived from the
// clock only (device unknown).
func (e *Engine) HomeHubs(ctx context.Context, user *core.User, limit int) ([]core.Hub, error) {
	return e.HomeHubsFor(ctx, user, core.NewHubContext(e.now().In(e.loc), "", "", ""), limit)
}

// SectionHubs implements core.Recommender.
func (e *Engine) SectionHubs(ctx context.Context, user *core.User, t core.MediaType, limit int) ([]core.Hub, error) {
	return e.SectionHubsFor(ctx, user, t, core.NewHubContext(e.now().In(e.loc), "", "", ""), limit)
}

// Related implements core.Recommender by combining the base recommender's
// detail-page rows with personalized context-aware shelves (Closer to Your
// Taste, Same Vibe Under 105 Minutes, and high-affinity craft/auteur matches)
// when the viewer's model is warm.
func (e *Engine) Related(ctx context.Context, user *core.User, key core.RatingKey, limit int) ([]core.Hub, error) {
	excluded, _, err := e.exclusions(ctx, user)
	if err != nil {
		return nil, err
	}
	fetch := limit
	if len(excluded) > 0 {
		fetch = min(max(limit, 12)*4, 1000)
	}
	var hubs []core.Hub
	if e.opts.Base != nil {
		hubs, err = e.opts.Base.Related(ctx, user, key, fetch)
		if err != nil {
			return nil, err
		}
	} else {
		items, serr := e.catalog.Similar(ctx, key, fetch)
		if serr != nil {
			return nil, serr
		}
		items = recommend.FilterTitles(items, user)
		if len(items) > 0 {
			hubs = []core.Hub{{ID: "movie.similar", Title: "More Like This", Context: "hub.movie.similar", Style: "shelf", Items: items}}
		}
	}
	if len(excluded) > 0 {
		for i := range hubs {
			items := make([]core.Title, 0, len(hubs[i].Items))
			for _, item := range hubs[i].Items {
				if !excluded.contains(&item) {
					items = append(items, item)
				}
			}
			hubs[i].Items = items
		}
	}
	hubs = e.enrichRelated(ctx, user, key, fetch, hubs, excluded)
	if len(excluded) == 0 {
		return hubs, nil
	}
	out := make([]core.Hub, 0, len(hubs))
	for _, h := range hubs {
		items := make([]core.Title, 0, len(h.Items))
		for _, item := range h.Items {
			if !excluded.contains(&item) {
				items = append(items, item)
				if limit > 0 && len(items) >= limit {
					break
				}
			}
		}
		if len(items) > 0 {
			h.Items = items
			out = append(out, h)
		}
	}
	return out, nil
}

// enrichRelated appends personalized detail shelves from the viewer's warm
// model without blocking on a rebuild.
func (e *Engine) enrichRelated(ctx context.Context, user *core.User, key core.RatingKey, limit int, base []core.Hub, excluded excludedTitles) []core.Hub {
	if user == nil || user.ID == 0 {
		return base
	}
	if limit <= 0 {
		limit = 12
	}
	e.mu.Lock()
	m := e.models[user.ID]
	e.mu.Unlock()
	if m != nil && e.opts.NotInterestedSnapshot != nil {
		signals, revision, err := e.opts.NotInterestedSnapshot(ctx, user)
		_, signature := snapshotSet(signals, revision)
		if err != nil || m.preferenceSignature != signature {
			return base
		}
	}
	if m == nil || m.generic || m.taste == nil || m.taste.Confidence < minPersonalConfidence {
		return base
	}
	tk := tasteKey(key)
	src, err := e.catalog.Get(ctx, tk)
	if err != nil || src == nil {
		return base
	}
	prefix := "movie"
	relType := core.TypeMovie
	if src.Type == core.TypeShow || src.Type == core.TypeSeason || src.Type == core.TypeEpisode {
		prefix, relType = "tv", core.TypeShow
	}
	seen := map[core.RatingKey]bool{key: true, tk: true}
	for _, h := range base {
		for _, it := range h.Items {
			seen[it.Key] = true
		}
	}
	var cands []core.Title
	seenCand := map[core.RatingKey]bool{key: true, tk: true}
	for _, r := range m.rows {
		for _, it := range r.items {
			if it.t.Type == relType && !excluded.contains(&it.t) && !seenCand[it.t.Key] && recommend.ContentRatingAllowed(it.t.ContentRating, user.MaxContentRating) {
				seenCand[it.t.Key] = true
				cands = append(cands, it.t)
			}
		}
	}
	type scored struct {
		t   core.Title
		val float64
	}
	// 1. "Closer to Your Taste": candidates from the viewer's personal rows
	// that share thematic/feature DNA with the opened title.
	var closer []scored
	for _, t := range cands {
		if seen[t.Key] {
			continue
		}
		sim := titleSimilarity(src, &t)
		fit, _ := m.taste.Fit(&t)
		if sim >= 0.18 && fit >= 0.15 {
			closer = append(closer, scored{t: t, val: 0.55*fit + 0.45*sim})
		}
	}
	if len(closer) >= 3 {
		sort.SliceStable(closer, func(i, j int) bool { return closer[i].val > closer[j].val })
		var items []core.Title
		for i := 0; i < len(closer) && i < limit; i++ {
			seen[closer[i].t.Key] = true
			items = append(items, closer[i].t)
		}
		base = append(base, core.Hub{
			ID: prefix + ".personal", Title: "Closer to Your Taste",
			Type: relType, Context: "hub." + prefix + ".personal", Style: "shelf", Items: items,
		})
	}
	// 2. "Same Vibe, Shorter Runtime": when the opened movie is 125+ minutes,
	// offer sub-105-minute matches in the same genre.
	if relType == core.TypeMovie && minutes(src) >= 125 {
		srcGenres := normGenres(src.Genres)
		var shorter []scored
		for _, t := range cands {
			if seen[t.Key] || minutes(&t) <= 0 || minutes(&t) > 105 {
				continue
			}
			if !anyShared(normGenres(t.Genres), srcGenres) {
				continue
			}
			fit, _ := m.taste.Fit(&t)
			if fit >= 0 {
				shorter = append(shorter, scored{t: t, val: 0.5*fit + 0.3*titleSimilarity(src, &t) + 0.2*quality(&t)})
			}
		}
		if len(shorter) >= 3 {
			sort.SliceStable(shorter, func(i, j int) bool { return shorter[i].val > shorter[j].val })
			var items []core.Title
			for i := 0; i < len(shorter) && i < limit; i++ {
				seen[shorter[i].t.Key] = true
				items = append(items, shorter[i].t)
			}
			base = append(base, core.Hub{
				ID: prefix + ".shorter", Title: "Same Vibe, Under 105 Minutes",
				Type: relType, Context: "hub." + prefix + ".shorter", Style: "shelf", Items: items,
			})
		}
	}
	return base
}

// HomeHubsFor implements core.ContextualRecommender: the viewer's Home rows
// for this context, served from the precomputed model. The Continue
// Watching row is returned as a placeholder ("home.continue", no items)
// marking where the Plex front end should put its own.
func (e *Engine) HomeHubsFor(ctx context.Context, user *core.User, hc core.HubContext, limit int) ([]core.Hub, error) {
	p := e.home(ctx, user, hc, "", limit, false)
	p.Rows = arrangeLayout(p.Rows, e.layoutForContext(ctx), userID(user), hc, sectionName(""), e.opts.MaxHomeRows)
	return toHubs(p.Rows), nil
}

// SectionHubsFor implements core.ContextualRecommender for a library
// section's rows (below the front end's Continue Watching and Recently
// Added).
func (e *Engine) SectionHubsFor(ctx context.Context, user *core.User, t core.MediaType, hc core.HubContext, limit int) ([]core.Hub, error) {
	p := e.home(ctx, user, hc, t, limit, false)
	p.Rows = arrangeLayout(p.Rows, e.layoutForContext(ctx), userID(user), hc, sectionName(t), e.opts.MaxSectionRows)
	return toHubs(p.Rows), nil
}

func toHubs(rows []core.HomeRow) []core.Hub {
	out := make([]core.Hub, 0, len(rows))
	for _, r := range rows {
		if r.Hidden {
			continue
		}
		h := r.Hub
		if r.Kind == KindContinue {
			h.Items = nil
		}
		out = append(out, h)
	}
	return out
}

// home is the one code path behind /hubs, /hubs/sections/{id} and the
// owner's Home preview.
func (e *Engine) home(ctx context.Context, user *core.User, hc core.HubContext, section core.MediaType, limit int, wait bool) core.HomePreview {
	return e.homeWithPolicy(ctx, user, hc, section, limit, wait, nil)
}

func (e *Engine) homeWithPolicy(ctx context.Context, user *core.User, hc core.HubContext, section core.MediaType, limit int, wait bool, allow func(context.Context, *core.User, *core.Title) bool) core.HomePreview {
	if limit <= 0 {
		limit = 12
	}
	if hc.Now.IsZero() {
		hc.Now = e.now().In(e.loc)
	}
	u := core.User{}
	if user != nil {
		u = *user
	}
	p := core.HomePreview{GeneratedAt: e.now(), Context: hc, Rows: []core.HomeRow{}}
	excluded, exclusionSig, exclusionErr := e.exclusions(ctx, &u)
	if exclusionErr != nil {
		e.log.Warn("personalize: recommendation exclusions unavailable", "user", u.ID, "err", exclusionErr)
		return p
	}
	m := e.modelFor(ctx, &u, wait)
	if m == nil {
		return p
	}
	stats := e.statsFor(ctx)
	var inprog []core.WatchStatus
	if section == "" && !m.generic && e.state != nil {
		var err error
		if inprog, err = e.state.InProgress(ctx, u.ID, 50); err != nil {
			e.log.Debug("personalize: in progress failed", "err", err)
		}
	}
	layoutDoc := e.layoutForContext(ctx)
	owner, ownerVer := e.ownerRows(ctx, &u, hc, section, layoutDoc, wait)
	layoutVer := e.layoutVersion()
	day := dayOf(hc.Now, e.loc)
	intent, intentVer := e.intentFor(u.ID, hc.Now)
	tune := resolveTuning(layoutDoc, u.ID, sectionName(section))
	// Movies / TV Shows pages keep what the Home leads with off the page
	// (the draft layout's switch and Home in a preview).
	var lead *homeLead
	if section != "" && resolveDedupeHome(layoutDoc, u.ID, sectionName(section)) {
		lead = e.homeLeadFor(ctx, &u, hc, layoutDoc, limit, wait, allow)
	}
	key := memoKey{client: hc.ClientID, session: hc.SessionID, user: u.ID, section: section, bucket: hc.Bucket(), day: day, hour: hc.Now.Hour(), limit: limit, model: m.version,
		layout: layoutVer, stats: stats.at.UnixNano(), owner: ownerVer, cw: progressSig(inprog),
		limits: u.MaxContentRating + "|" + fmt.Sprint(u.AllowedSections, u.IsOwner), intent: intentVer, tuning: tune.key(), exclusions: exclusionSig}
	if lead != nil {
		key.lead = lead.sig
	}
	_, draft := ctx.Value(draftLayoutKey{}).(core.HomeLayout)
	var policyContinue []candItem
	if allow != nil && section == "" {
		policyContinue = e.continueFrom(ctx, inprog)
	}
	hidden := map[core.RatingKey]bool{}
	if e.opts.FilterAvailable != nil || allow != nil {
		var pool []core.Title
		seen := map[core.RatingKey]bool{}
		for _, row := range append(append([]*candRow{}, m.rows...), owner...) {
			for _, it := range row.items {
				pool = append(pool, it.t)
				hidden[it.t.Key] = e.opts.FilterAvailable != nil
			}
			for _, items := range row.typed {
				for _, it := range items {
					pool = append(pool, it.t)
					hidden[it.t.Key] = e.opts.FilterAvailable != nil
				}
			}
		}
		if e.opts.FilterAvailable != nil {
			for _, t := range e.opts.FilterAvailable(ctx, pool) {
				delete(hidden, t.Key)
			}
		}
		if allow != nil {
			for _, candidate := range policyContinue {
				pool = append(pool, candidate.t)
			}
			for i := range pool {
				t := &pool[i]
				if !seen[t.Key] {
					seen[t.Key] = true
					if ctx.Err() != nil || !allow(ctx, &u, t) {
						hidden[t.Key] = true
					}
				}
			}
		}
		keys := make([]int64, 0, len(hidden))
		for k, blocked := range hidden {
			if blocked {
				keys = append(keys, int64(k))
			}
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		key.limits += fmt.Sprint(keys)
	}

	if ent, ok := e.memoGet(key); ok && !draft {
		e.touchPresentation(key, m.generic)
		p.Rows, p.HomeDeduped = ent.rows, ent.deduped
		return p
	}
	a := &assembly{
		minRow: max(e.opts.MinRowItems, 0),
		hidden: hidden, excluded: excluded,
		layout: &layoutDoc,
		model:  m, user: u, hc: hc, section: section, limit: limit, owner: owner,
		global: stats.global, arms: stats.users[u.ID], allowed: e.allowedFor(&u), explore: e.opts.Exploration, day: day,
		intent: intent, tune: &tune, lead: lead,
	}
	a.explore = clamp(a.explore*tune.exploreScale(), 0, 1)
	if section == "" {
		a.pageLen = e.opts.MaxHomeRows
		if allow != nil {
			a.cw = policyContinue
		} else {
			a.cw = e.continueFrom(ctx, inprog)
		}
	} else {
		a.pageLen = e.opts.MaxSectionRows
	}
	// Every row is assembled (the preview explains the ones the page cuts),
	// but only the rows that survive the page length claim titles (fill).
	a.maxRows = 100
	a.rk = e.newRankPass(u, hc, section, draft) // learned ranker (ranker.go)
	p.Rows = e.assemblePage(a, key, draft)
	p.HomeDeduped = a.homeDeduped
	a.rk.publishRows(p.Rows) // feature snapshots for the serve log (ranker.go)
	for _, r := range p.Rows {
		e.registerHub(r.HubID, r.Kind)
	}
	if !draft {
		e.memoPut(key, memoEntry{rows: p.Rows, deduped: p.HomeDeduped})
	}
	return p
}

// ---- assembled-rows memo ------------------------------------------------------

// memoKey identifies one assembly: everything its result depends on. A
// new model, layout, statistics snapshot, owner rows or Continue Watching
// change produces a new key, so entries never need invalidating.
type memoKey struct {
	client, session string
	user            int64
	section         core.MediaType
	bucket          string
	day             int
	hour            int // layout schedules have hour windows
	limit           int
	model           uint64
	layout          uint64
	stats           int64
	owner           int64
	cw              string
	limits          string
	intent          uint64 // live session-intent version (#32); 0 = none
	tuning          string // the page's resolved dials
	lead            uint64 // what the Home leads with (Movies/TV dedupe); 0 = none
	exclusions      string // current explicit recommendation exclusions
}

type memoEntry struct {
	rows    []core.HomeRow
	deduped int
	at      time.Time
}

// memoTTL bounds how long an assembled screen is reused; clients poll
// /hubs often and the rows only change when an input does.
const memoTTL = 2 * time.Minute

func (e *Engine) memoGet(k memoKey) (memoEntry, bool) {
	e.memoMu.Lock()
	defer e.memoMu.Unlock()
	ent, ok := e.memo[k]
	if !ok || e.now().Sub(ent.at) > memoTTL {
		return memoEntry{}, false
	}
	// Callers may append to or reorder the slice; the rows themselves are
	// read-only.
	return memoEntry{rows: append([]core.HomeRow(nil), ent.rows...), deduped: ent.deduped, at: ent.at}, true
}

func (e *Engine) memoPut(k memoKey, ent memoEntry) {
	rows := ent.rows
	e.memoMu.Lock()
	defer e.memoMu.Unlock()
	now := e.now()
	if len(e.memo) >= 4096 {
		for mk, ent := range e.memo {
			if now.Sub(ent.at) > memoTTL {
				delete(e.memo, mk)
			}
		}
		if len(e.memo) >= 4096 {
			e.memo = map[memoKey]*memoEntry{}
		}
	}
	e.memo[k] = &memoEntry{rows: append([]core.HomeRow(nil), rows...), deduped: ent.deduped, at: now}
}

// progressSig summarises Continue Watching for the memo key.
func progressSig(sts []core.WatchStatus) string {
	var b strings.Builder
	for _, ws := range sts {
		b.WriteString(ws.Key.String())
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(ws.LastViewedAt.Unix(), 36))
		b.WriteByte(',')
	}
	return b.String()
}

// allowedFor returns the media types a viewer's allowed sections cover
// (nil = everything).
func (e *Engine) allowedFor(u *core.User) func(core.MediaType) bool {
	if u == nil || u.IsOwner || len(u.AllowedSections) == 0 {
		return nil
	}
	ok := map[core.MediaType]bool{}
	for _, id := range u.AllowedSections {
		if t, found := e.opts.Sections[id]; found {
			ok[t] = true
		}
	}
	return func(t core.MediaType) bool { return ok[t] }
}

// layoutVersion counts saved layout changes; it keys the row memo.
func (e *Engine) layoutVersion() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.layoutVer
}

// ---- core.PersonalScorer ---------------------------------------------------------

// ScoreTitles implements core.PersonalScorer: taste fit blended with
// quality, roughly -0.7..1 (higher = better fit for this viewer).
func (e *Engine) ScoreTitles(ctx context.Context, user *core.User, titles []core.Title) (map[core.RatingKey]float64, error) {
	var ts *Taste
	if user != nil {
		e.mu.Lock()
		if m := e.models[user.ID]; m != nil {
			ts = m.taste
		}
		e.mu.Unlock()
		if ts == nil {
			var err error
			if ts, _, err = e.buildTaste(ctx, *user); err != nil {
				return nil, err
			}
		}
	}
	out := make(map[core.RatingKey]float64, len(titles))
	for i := range titles {
		fit, _ := ts.Fit(&titles[i])
		out[titles[i].Key] = round3(0.7*fit + 0.3*quality(&titles[i]))
	}
	return out, nil
}

func userID(u *core.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}
