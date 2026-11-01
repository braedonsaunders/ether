package newznab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Manager holds all configured indexers and spreads searches across
// them within their budgets. It implements core.Indexer so the
// resolver can treat the fleet as one indexer; FetchNZB routes by
// the release's Indexer name.
type Manager struct {
	events eventBus
	db     *sql.DB
	opts   Options
	log    *slog.Logger
	par    int

	mu             sync.RWMutex
	clients        []*Client
	searchTimingMu sync.RWMutex
	searchTimings  []SearchTiming
}

// SearchTiming records the most recent aggregate search's per-indexer cost.
type SearchTiming struct {
	Indexer  string
	Duration time.Duration
	Err      string
}

// SearchTimings returns a copy of the most recent per-indexer search timings.
func (m *Manager) SearchTimings() []SearchTiming {
	m.searchTimingMu.RLock()
	defer m.searchTimingMu.RUnlock()
	return append([]SearchTiming(nil), m.searchTimings...)
}

var _ core.Indexer = (*Manager)(nil)

// NewManager builds a Manager from indexer configs.
func NewManager(db *sql.DB, cfgs []config.Indexer, opts Options) *Manager {
	m := &Manager{db: db, opts: opts, par: opts.MaxParallel}
	m.log = opts.Logger
	if m.log == nil {
		m.log = slog.Default()
	}
	m.Reload(cfgs)
	return m
}

// Reload replaces the indexer set (call on config change). Existing
// clients with an unchanged ID keep their in-memory caches.
func (m *Manager) Reload(cfgs []config.Indexer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := map[string]*Client{}
	for _, cl := range m.clients {
		old[cl.indexerID()] = cl
	}
	next := make([]*Client, 0, len(cfgs))
	for _, cfg := range cfgs {
		id := cfg.ID
		if id == "" {
			id = cfg.Name
		}
		if cl, ok := old[id]; ok {
			cl.UpdateConfig(cfg)
			next = append(next, cl)
			continue
		}
		next = append(next, New(cfg, m.db, m.opts))
	}
	sort.SliceStable(next, func(i, j int) bool {
		return next[i].Config().Priority < next[j].Config().Priority
	})
	m.clients = next
}

// ApplyConfig reloads indexers and the search-cache TTL from a full
// config snapshot; subscribe it to config.Store changes.
func (m *Manager) ApplyConfig(cfg config.Config) {
	m.opts.Identity = cfg.ClientIdentity
	d := cfg.Resolver.SearchCacheTTL.D()
	if d > 0 {
		m.opts.SearchCacheTTL = d
	}
	for _, cl := range m.Clients() {
		cl.SetIdentityPolicy(cfg.ClientIdentity)
		if d > 0 {
			cl.SetCacheTTL(d)
		}
	}
	m.Reload(cfg.Indexers)
}

// Clients returns the enabled and disabled clients in priority order.
func (m *Manager) Clients() []*Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*Client(nil), m.clients...)
}

// Indexers returns the clients as core.Indexer values.
func (m *Manager) Indexers() []core.Indexer {
	cs := m.Clients()
	out := make([]core.Indexer, len(cs))
	for i, c := range cs {
		out[i] = c
	}
	return out
}

// Name implements core.Indexer.
func (m *Manager) Name() string { return "indexers" }

// MissingSources reports core.ErrNoSources when no indexer is enabled:
// a search would return nothing whatever the title.
func (m *Manager) MissingSources() error {
	for _, cl := range m.Clients() {
		if cl.Config().Enabled {
			return nil
		}
	}
	return core.ErrNoSources
}

// HasTorrentIndexer reports whether an enabled indexer returns torrents
// (Torznab, a Stremio addon or a torrent built-in site).
func (m *Manager) HasTorrentIndexer() bool {
	for _, cl := range m.Clients() {
		if cfg := cl.Config(); cfg.Enabled && cfg.IsTorrent() {
			return true
		}
	}
	return false
}

// Remaining returns budget state for every indexer.
func (m *Manager) Remaining(ctx context.Context) ([]Remaining, error) {
	var out []Remaining
	for _, cl := range m.Clients() {
		r, err := cl.Remaining(ctx)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Search implements core.Indexer: it queries enabled indexers in parallel
// (up to MaxParallel), letting each client check its cache before budgets, merges by priority order
// and de-duplicates by normalised name + size.
func (m *Manager) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	return m.SearchWith(ctx, req, nil)
}

// SearchRescue runs a second-pass rescue fan-out across all enabled indexers
// when the primary Search results had no playable candidate.
func (m *Manager) SearchRescue(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	return m.SearchRescueWith(ctx, req, nil)
}

// SearchRescueWith is SearchRescue with per-indexer early arrival callbacks.
func (m *Manager) SearchRescueWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	clients := m.Clients()
	type slot struct {
		results []core.Release
		err     error
	}
	slots := make([]slot, len(clients))
	parallel := m.par
	if parallel <= 0 {
		parallel = max(1, len(clients))
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, cl := range clients {
		if !cl.Config().Enabled {
			continue
		}
		wg.Add(1)
		go func(i int, cl *Client) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				slots[i].err = ctx.Err()
				return
			}
			res, err := cl.SearchRescue(ctx, req)
			slots[i].results, slots[i].err = res, err
			if arrived != nil && err == nil && len(res) > 0 {
				arrived(res)
			}
		}(i, cl)
	}
	wg.Wait()
	seen := map[string]bool{}
	var merged []core.Release
	for _, s := range slots {
		for _, r := range s.results {
			key := strings.ToLower(strings.TrimSpace(r.Name)) + "|" + fmt.Sprint(r.Size)
			if r.Size == 0 {
				key = strings.ToLower(strings.TrimSpace(r.Name))
			}
			if r.Protocol != core.ProtocolUsenet {
				key = r.Protocol + "|" + key
				if r.InfoHash != "" {
					key = r.Protocol + "|" + r.InfoHash
				}
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, r)
		}
	}
	return merged, nil
}

// SearchWith is Search, also handing each indexer's results to arrived as
// soon as that indexer answers (concurrently, from its own goroutine), so
// callers can start follow-up work, such as debrid cache checks, before
// the slowest indexer is done.
func (m *Manager) SearchWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	clients := m.Clients()
	type slot struct {
		results  []core.Release
		err      error
		skipped  bool
		duration time.Duration
	}
	slots := make([]slot, len(clients))
	parallel := m.par
	if parallel <= 0 {
		parallel = max(1, len(clients))
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, cl := range clients {
		if !cl.Config().Enabled {
			slots[i].skipped = true
			continue
		}
		wg.Add(1)
		go func(i int, cl *Client) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				slots[i].err = ctx.Err()
				return
			}
			started := time.Now()
			res, err := cl.SearchWith(ctx, req, arrived)
			slots[i].duration = time.Since(started)
			slots[i].results, slots[i].err = res, err
		}(i, cl)
	}
	wg.Wait()
	timings := make([]SearchTiming, 0, len(clients))
	for i, s := range slots {
		now := time.Now()
		var age time.Duration
		var dated int
		for _, r := range s.results {
			if !r.PubDate.IsZero() && now.After(r.PubDate) {
				age += now.Sub(r.PubDate)
				dated++
			}
		}
		if dated > 0 {
			age /= time.Duration(dated)
		}
		if (!s.skipped || errors.Is(s.err, core.ErrBudgetExhausted)) && !errors.Is(s.err, context.Canceled) {
			m.emitEvent(Event{Indexer: clients[i].Name(), Kind: "search", Success: s.err == nil, BudgetExhausted: errors.Is(s.err, core.ErrBudgetExhausted), Results: len(s.results), Duration: s.duration, AverageAge: age, At: now})
		}
		if !s.skipped {
			label := clients[i].Name()
			e := ""
			if s.err != nil {
				e = s.err.Error()
			}
			timings = append(timings, SearchTiming{Indexer: label, Duration: s.duration, Err: e})
		}
	}
	m.searchTimingMu.Lock()
	m.searchTimings = timings
	m.searchTimingMu.Unlock()
	if err := ctx.Err(); err != nil {
		// Still return whatever arrived if anything did.
	}
	seen := map[string]bool{}
	var merged []core.Release
	var errs []error
	anySearched := false
	for i, s := range slots {
		if s.err != nil {
			errs = append(errs, s.err)
			m.log.Debug("indexer search failed", "indexer", clients[i].Name(), "err", s.err)
		}
		if !s.skipped {
			anySearched = true
		}
		for _, r := range s.results {
			key := strings.ToLower(strings.TrimSpace(r.Name)) + "|" + fmt.Sprint(r.Size)
			if r.Size == 0 {
				key = strings.ToLower(strings.TrimSpace(r.Name))
			}
			if r.Protocol != core.ProtocolUsenet {
				// A torrent and a Usenet post of the same name are distinct
				// sources; torrents dedupe by info hash when known.
				key = r.Protocol + "|" + key
				if r.InfoHash != "" {
					key = r.Protocol + "|" + r.InfoHash
				}
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, r)
		}
	}
	if len(merged) == 0 && len(errs) > 0 {
		allBudget := true
		for _, e := range errs {
			if !errors.Is(e, core.ErrBudgetExhausted) {
				allBudget = false
			}
		}
		if allBudget {
			return nil, fmt.Errorf("all indexers: %w", core.ErrBudgetExhausted)
		}
		if !anySearched && len(clients) > 0 {
			return nil, errs[0]
		}
		if anySearched {
			// Some indexers errored and none returned results.
			return nil, errs[0]
		}
	}
	return merged, nil
}

// FetchNZB implements core.Indexer by routing to the originating client.
func (m *Manager) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	for _, cl := range m.Clients() {
		if cl.Name() == r.Indexer {
			started := time.Now()
			b, err := cl.FetchNZB(ctx, r)
			m.emitEvent(Event{Indexer: cl.Name(), Kind: "grab", Success: err == nil, BudgetExhausted: errors.Is(err, core.ErrBudgetExhausted), Duration: time.Since(started), At: time.Now()})
			return b, err
		}
	}
	// Unknown origin (e.g. cached release after a rename): try each
	// enabled client in priority order.
	var firstErr error
	for _, cl := range m.Clients() {
		if !cl.Config().Enabled {
			continue
		}
		started := time.Now()
		b, err := cl.FetchNZB(ctx, r)
		m.emitEvent(Event{Indexer: cl.Name(), Kind: "grab", Success: err == nil, BudgetExhausted: errors.Is(err, core.ErrBudgetExhausted), Duration: time.Since(started), At: time.Now()})
		if err == nil {
			return b, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if errors.Is(err, core.ErrBudgetExhausted) {
			continue
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("newznab: no indexer available to fetch %q", r.Name)
}
