package collections

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/lru"
)

// Ephemeral collections are the browseable grids synthesized from vibe
// searches ("90s thrillers under 100 mins"). They live only in memory:
// per viewer, in a bounded LRU with a TTL. Their keys are KindSmart
// keys in the reserved ephemeral payload range (core.EphemeralSmartBase up to core.EphemeralSmartEnd)
// so every existing collection path serves them. Rating one promotes
// it to a persisted personal smart collection (PromoteEphemeral); the
// ephemeral key then aliases the saved one until it is evicted or the
// server restarts. Unrating never deletes anything.

const (
	defaultVibeTTL = 6 * time.Hour
	defaultVibeMax = 512
	vibeTopItems   = 50
)

// WithVibeRefiner installs the optional LLM refiner consulted when the
// deterministic parser cannot read a query.
func WithVibeRefiner(r core.VibeRefiner) Option {
	return func(s *Service) { s.SetVibeRefiner(r) }
}

// WithEphemeralLimits bounds the ephemeral registry (tests).
func WithEphemeralLimits(ttl time.Duration, max int) Option {
	return func(s *Service) { s.vibeTTL, s.vibeMax = ttl, max }
}

// SetVibeRefiner installs the refiner after construction.
func (s *Service) SetVibeRefiner(r core.VibeRefiner) {
	s.depsMu.Lock()
	s.vibeRefiner = r
	s.depsMu.Unlock()
}

func (s *Service) vibeRefinerDep() core.VibeRefiner {
	s.depsMu.RLock()
	defer s.depsMu.RUnlock()
	return s.vibeRefiner
}

type ephemeralEntry struct {
	key        core.RatingKey
	owner      int64
	vibe       core.VibeQuery
	created    time.Time
	promoted   core.RatingKey
	childCount int
	poster     string
	backdrop   string
}

type vibeRegistry struct {
	mu      sync.Mutex
	byKey   *lru.Cache[core.RatingKey, *ephemeralEntry]
	byQuery map[string]core.RatingKey
	seq     atomic.Int64
	ttl     time.Duration
	max     int
}

func newVibeRegistry(ttl time.Duration, max int) *vibeRegistry {
	if ttl <= 0 {
		ttl = defaultVibeTTL
	}
	if max <= 0 {
		max = defaultVibeMax
	}
	r := &vibeRegistry{byQuery: map[string]core.RatingKey{}, ttl: ttl, max: max}
	r.byKey = lru.New(lru.Options[core.RatingKey, *ephemeralEntry]{MaxEntries: max, OnEvict: func(k core.RatingKey, _ *ephemeralEntry) {
		r.forgetQueriesLocked(k)
	}})
	return r
}

func (s *Service) vibeReg() *vibeRegistry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vibe == nil {
		s.vibe = newVibeRegistry(s.vibeTTL, s.vibeMax)
	}
	return s.vibe
}

func (r *vibeRegistry) nextKey() core.RatingKey {
	return core.EphemeralSmartKey(r.seq.Add(1))
}

func (r *vibeRegistry) expired(e *ephemeralEntry, now time.Time) bool {
	// Promoted aliases do not expire on the clock; the LRU still
	// bounds them.
	return e.promoted == 0 && now.Sub(e.created) > r.ttl
}

func (r *vibeRegistry) removeLocked(key core.RatingKey) {
	r.byKey.Remove(key)
	r.forgetQueriesLocked(key)
}

func (r *vibeRegistry) forgetQueriesLocked(key core.RatingKey) {
	for sig, k := range r.byQuery {
		if k == key {
			delete(r.byQuery, sig)
		}
	}
}

// get returns a copy of the live entry for key, marking it used.
func (r *vibeRegistry) get(key core.RatingKey, now time.Time) (*ephemeralEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byKey.Get(key)
	if !ok {
		return nil, false
	}
	if r.expired(e, now) {
		r.removeLocked(key)
		return nil, false
	}
	cp := *e
	return &cp, true
}

func (r *vibeRegistry) bySig(sig string, now time.Time) (*ephemeralEntry, bool) {
	r.mu.Lock()
	key, ok := r.byQuery[sig]
	r.mu.Unlock()
	if !ok {
		return nil, false
	}
	return r.get(key, now)
}

func (r *vibeRegistry) put(sig string, e *ephemeralEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey.Add(e.key, e)
	r.byQuery[sig] = e.key
}

// update mutates a live entry in place.
func (r *vibeRegistry) update(key core.RatingKey, fn func(*ephemeralEntry)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byKey.Get(key); ok {
		fn(e)
	}
}

// vibeSig identifies a viewer's resolved vibe for de-duplication.
func vibeSig(owner int64, v core.VibeQuery) string {
	b, _ := json.Marshal(struct {
		F core.Filter
		S core.SortOrder
		D bool
		K core.RatingKey
	}{v.Filter, v.Sort, v.Desc, v.SimilarKey})
	return fmt.Sprintf("%d|%s", owner, b)
}

func vibeOwner(user *core.User) int64 {
	if user == nil {
		return 0
	}
	return user.ID
}

// VibeCollection implements core.VibeSearcher.
func (s *Service) VibeCollection(ctx context.Context, user *core.User, query string) (*core.Collection, []core.Title, bool, error) {
	v, ok := ParseVibe(query)
	if !ok {
		if r := s.vibeRefinerDep(); r != nil {
			bctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
			rv, rok, err := r.RefineVibe(bctx, user, query)
			cancel()
			if err == nil && rok {
				v, ok = rv, true
				if v.Query == "" {
					v.Query = query
				}
			}
		}
	}
	if !ok {
		return nil, nil, false, nil
	}
	if err := s.resolveVibe(ctx, &v); err != nil {
		// An unresolvable person/seed means it probably was a title
		// search after all; let plain search have it.
		s.log.Debug("collections: vibe unresolved", "query", query, "err", err)
		return nil, nil, false, nil
	}
	owner := vibeOwner(user)
	sig := vibeSig(owner, v)
	reg := s.vibeReg()
	if e, found := reg.bySig(sig, s.now()); found {
		page, err := s.ephemeralPage(ctx, user, e, 0, vibeTopItems)
		if err != nil {
			return nil, nil, false, err
		}
		if page.Total == 0 {
			return nil, nil, false, nil
		}
		s.rememberEphemeral(e, page)
		c := ephemeralCollection(e, page.Total)
		return &c, page.Items, true, nil
	}
	e := &ephemeralEntry{key: reg.nextKey(), owner: owner, vibe: v, created: s.now()}
	page, err := s.ephemeralPage(ctx, user, e, 0, vibeTopItems)
	if err != nil {
		return nil, nil, false, err
	}
	if page.Total == 0 {
		return nil, nil, false, nil
	}
	s.rememberEphemeral(e, page)
	reg.put(sig, e)
	c := ephemeralCollection(e, page.Total)
	return &c, page.Items, true, nil
}

// resolveVibe fills in Filter.Person / SimilarKey / studio names.
func (s *Service) resolveVibe(ctx context.Context, v *core.VibeQuery) error {
	if v.PersonName != "" && v.Filter.Person == 0 {
		bctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		r, err := s.names.resolve(bctx, "person", ref{Name: v.PersonName}, "")
		cancel()
		if err == nil && r.ID > 0 {
			v.Filter.Person = r.ID
			if r.Name != "" {
				v.Title = r.Name + " " + mediaNoun(v.Filter.Type, "Films")
			}
			v.PersonName = ""
		} else if studio, found := s.studioByName(ctx, v.PersonName); found {
			v.Filter.Studio = studio
			v.Title = studio + " " + mediaNoun(v.Filter.Type, "Movies")
			v.PersonName = ""
		} else {
			return fmt.Errorf("person %q: no catalogue match", v.PersonName)
		}
	}
	if v.SimilarSeed != "" && v.SimilarKey == 0 {
		var types []core.MediaType
		if v.Filter.Type != "" {
			types = []core.MediaType{v.Filter.Type}
		}
		hits, err := s.cat.Search(ctx, v.SimilarSeed, types, 5)
		if err != nil {
			return err
		}
		seed, found := pickSeed(hits, v.SimilarSeed)
		if !found {
			return fmt.Errorf("seed %q: no catalogue match", v.SimilarSeed)
		}
		v.SimilarKey = seed.Key
		if v.Filter.Type == "" {
			v.Filter.Type = seed.Type
		}
		v.Title = mediaNoun(v.Filter.Type, "Titles") + " Like " + seed.Title
	}
	if v.Title == "" {
		v.Title = "Generated Collection"
	}
	if v.Sort == "" {
		v.Sort, v.Desc = core.SortPopularity, true
	}
	return nil
}

// pickSeed prefers an exact (normalized) title match over the first hit.
func pickSeed(hits []core.Title, seed string) (core.Title, bool) {
	want := normName(seed)
	for _, h := range hits {
		if normName(h.Title) == want {
			return h, true
		}
	}
	if len(hits) > 0 {
		return hits[0], true
	}
	return core.Title{}, false
}

// studioByName matches a name against the catalogue's studio facet.
func (s *Service) studioByName(ctx context.Context, name string) (string, bool) {
	facets, err := s.cat.Facet(ctx, "", "studio")
	if err != nil {
		return "", false
	}
	want := normName(name)
	for _, f := range facets {
		if normName(f.Key) == want {
			return f.Key, true
		}
	}
	return "", false
}

func (r *vibeRegistry) listFor(owner int64, now time.Time) []*ephemeralEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey.RemoveFunc(func(k core.RatingKey, e *ephemeralEntry) bool {
		if !r.expired(e, now) {
			return false
		}
		r.forgetQueriesLocked(k)
		return true
	})
	var out []*ephemeralEntry
	for _, e := range r.byKey.All() {
		if e.owner == owner && e.promoted == 0 {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out
}

// ephemeralCollection renders an entry as a core.Collection.
func ephemeralCollection(e *ephemeralEntry, total int) core.Collection {
	f := e.vibe.Filter
	f.MaxContentRating = ""
	typ := f.Type
	if typ == "" {
		typ = core.TypeMovie
	}
	return core.Collection{
		Key: e.key, Title: e.vibe.Title, Kind: "smart", Type: typ,
		Summary:     fmt.Sprintf("Generated from your search for %q. Rate it to keep it as your own collection.", e.vibe.Query),
		Filter:      &f,
		Sort:        e.vibe.Sort,
		OwnerUserID: e.owner,
		PosterPath:  e.poster, BackdropPath: e.backdrop,
		ChildCount: total, UpdatedAt: e.created,
	}
}

// rememberEphemeral caches count and artwork from a served page.
func (s *Service) rememberEphemeral(e *ephemeralEntry, page core.Page) {
	poster, backdrop := e.poster, e.backdrop
	for _, t := range page.Items {
		if poster == "" {
			poster = t.PosterPath
		}
		if backdrop == "" {
			backdrop = t.BackdropPath
		}
		if poster != "" && backdrop != "" {
			break
		}
	}
	e.childCount, e.poster, e.backdrop = page.Total, poster, backdrop
	s.vibeReg().update(e.key, func(live *ephemeralEntry) {
		live.childCount, live.poster, live.backdrop = page.Total, poster, backdrop
	})
}

// ephemeralPage computes members live: Browse with the vibe filter, or
// the catalogue's Similar list filtered in memory for "like X" vibes.
// The viewer's content-rating ceiling is applied on every serve.
func (s *Service) ephemeralPage(ctx context.Context, user *core.User, e *ephemeralEntry, offset, limit int) (core.Page, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	f := e.vibe.Filter
	if user != nil {
		f.MaxContentRating = user.MaxContentRating
	}
	if e.vibe.SimilarKey != 0 {
		all, err := s.cat.Similar(ctx, e.vibe.SimilarKey, 100)
		if err != nil {
			return core.Page{}, err
		}
		var kept []core.Title
		for _, t := range all {
			if t.Key != e.vibe.SimilarKey && vibeTitleMatches(t, f) {
				kept = append(kept, t)
			}
		}
		total := len(kept)
		if offset >= total {
			return core.Page{Items: []core.Title{}, Total: total}, nil
		}
		return core.Page{Items: kept[offset:pageEnd(offset, limit, total)], Total: total}, nil
	}
	return s.cat.Browse(ctx, core.BrowseQuery{
		Filter: f, Sort: e.vibe.Sort, Desc: e.vibe.Desc,
		Offset: offset, Limit: limit,
	})
}

// vibeTitleMatches applies a vibe filter to one title in memory (the
// Similar path). Unknown fields on the title never exclude it.
func vibeTitleMatches(t core.Title, f core.Filter) bool {
	if t.Adult {
		return false
	}
	if f.Type != "" && t.Type != f.Type {
		return false
	}
	if len(f.Genres) > 0 && len(t.Genres) > 0 && !anyIn(t.Genres, f.Genres) {
		return false
	}
	if f.YearFrom > 0 && t.Year > 0 && t.Year < f.YearFrom {
		return false
	}
	if f.YearTo > 0 && t.Year > 0 && t.Year > f.YearTo {
		return false
	}
	if f.MinRating > 0 && t.Rating > 0 && t.Rating < f.MinRating {
		return false
	}
	if f.MaxRuntime > 0 && t.Runtime > 0 && t.Runtime > f.MaxRuntime {
		return false
	}
	if f.MinRuntime > 0 && t.Runtime > 0 && t.Runtime < f.MinRuntime {
		return false
	}
	if len(f.Languages) > 0 && t.Language != "" && !stringsIn(f.Languages, t.Language) {
		return false
	}
	if len(f.Countries) > 0 && len(t.Countries) > 0 && !anyIn(t.Countries, f.Countries) {
		return false
	}
	if f.MaxContentRating != "" && contentrating.Exceeds(t.ContentRating, f.MaxContentRating) {
		return false
	}
	return true
}

func anyIn(have, want []string) bool {
	for _, h := range have {
		if stringsIn(want, h) {
			return true
		}
	}
	return false
}

func stringsIn(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

// getEphemeral serves Collections.Get for an ephemeral key.
func (s *Service) getEphemeral(ctx context.Context, user *core.User, key core.RatingKey) (*core.Collection, error) {
	e, ok := s.vibeReg().get(key, s.now())
	if !ok || e.owner != vibeOwner(user) {
		return nil, core.ErrNotFound
	}
	if e.promoted != 0 {
		return s.Get(ctx, user, e.promoted)
	}
	page, err := s.ephemeralPage(ctx, user, e, 0, 1)
	if err != nil {
		return nil, err
	}
	s.rememberEphemeral(e, page)
	c := ephemeralCollection(e, page.Total)
	if len(page.Items) > 0 && e.vibe.Filter.Type == "" {
		c.Type = page.Items[0].Type
	}
	return &c, nil
}

// ephemeralItems serves Collections.Items for an ephemeral key.
func (s *Service) ephemeralItems(ctx context.Context, user *core.User, key core.RatingKey, offset, limit int) (core.Page, error) {
	e, ok := s.vibeReg().get(key, s.now())
	if !ok || e.owner != vibeOwner(user) {
		return core.Page{}, core.ErrNotFound
	}
	if e.promoted != 0 {
		return s.Items(ctx, user, e.promoted, offset, limit)
	}
	page, err := s.ephemeralPage(ctx, user, e, offset, limit)
	if err != nil {
		return core.Page{}, err
	}
	s.rememberEphemeral(e, page)
	return page, nil
}

// listEphemeral serves the owner's live ephemeral collections in List.
func (s *Service) listEphemeral(user *core.User, t core.MediaType) []core.Collection {
	var out []core.Collection
	for _, e := range s.vibeReg().listFor(vibeOwner(user), s.now()) {
		if t != "" && e.vibe.Filter.Type != "" && e.vibe.Filter.Type != t {
			continue
		}
		c := ephemeralCollection(e, e.childCount)
		if t != "" {
			c.Type = t
		}
		out = append(out, c)
	}
	return out
}

// PromoteEphemeral implements core.VibeSearcher: rating an ephemeral
// collection saves it as a persisted personal smart collection with
// the same title. The ephemeral key keeps working as an alias.
func (s *Service) PromoteEphemeral(ctx context.Context, user *core.User, key core.RatingKey) (*core.Collection, bool, error) {
	if !key.IsEphemeral() || user == nil {
		return nil, false, nil
	}
	e, ok := s.vibeReg().get(key, s.now())
	if !ok || e.owner != user.ID {
		return nil, false, nil
	}
	if e.promoted != 0 {
		c, err := s.Get(ctx, user, e.promoted)
		return c, err == nil, err
	}
	f := e.vibe.Filter
	f.MaxContentRating = ""
	if e.vibe.SimilarKey != 0 {
		// A "like X" vibe has no filter form; save the seed's genres
		// and type with the vibe's other constraints instead.
		seed, err := s.cat.Get(ctx, e.vibe.SimilarKey)
		if err != nil {
			return nil, false, err
		}
		f.Type = seed.Type
		if len(f.Genres) == 0 {
			f.Genres = seed.Genres
		}
	}
	created, err := s.CreateSmart(ctx, core.Collection{
		Title:   e.vibe.Title,
		Summary: fmt.Sprintf("Saved from your search for %q.", e.vibe.Query),
		Filter:  &f, Sort: e.vibe.Sort, Type: f.Type, OwnerUserID: user.ID,
	})
	if err != nil {
		return nil, false, err
	}
	s.vibeReg().update(key, func(live *ephemeralEntry) { live.promoted = created.Key })
	return created, true, nil
}

var _ core.VibeSearcher = (*Service)(nil)
