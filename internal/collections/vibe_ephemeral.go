package collections

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/lru"
)

// Ephemeral collections are the browseable grids synthesized from vibe
// searches once the configured AI has generated a collection spec. They live only in memory:
// per viewer, in a bounded LRU with a TTL. Their keys are KindSmart
// keys in the reserved ephemeral payload range (core.EphemeralSmartBase up to core.EphemeralSmartEnd)
// so every existing collection path serves them. Rating one promotes
// it to a persisted personal smart collection (PromoteEphemeral); the
// ephemeral key then aliases the saved one until it is evicted or the
// server restarts. Unrating never deletes anything.

const (
	defaultVibeTTL = 6 * time.Hour
	defaultVibeMax = 512
)

type ephemeralEntry struct {
	key         core.RatingKey
	owner       int64
	spec        core.CollectionSpec
	created     time.Time
	promoted    core.RatingKey
	childCount  int
	descriptive string
	poster      string
	backdrop    string
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

func vibeOwner(user *core.User) int64 {
	if user == nil {
		return 0
	}
	return user.ID
}

// VibeCollection implements core.VibeSearcher.
func (s *Service) VibeCollection(ctx context.Context, user *core.User, query string) (*core.Collection, []core.Title, bool, error) {
	result, err := s.DescribeSearch(ctx, user, query, false)
	if err != nil || result.State != "ready" || result.Spec == nil || len(result.Items) == 0 {
		return nil, nil, false, nil
	}

	reg := s.vibeReg()
	sig := fmt.Sprintf("ai:%d|%s", vibeOwner(user), strings.Join(strings.Fields(strings.ToLower(query)), " "))
	e, found := reg.bySig(sig, s.now())
	if !found {
		e = &ephemeralEntry{key: reg.nextKey(), owner: vibeOwner(user), created: s.now(), descriptive: query, spec: *result.Spec}
		reg.put(sig, e)
	}
	s.rememberEphemeral(e, core.Page{Items: result.Items, Total: len(result.Items)})
	c := ephemeralCollection(e, len(result.Items))
	c.Type = result.Items[0].Type
	return &c, result.Items, true, nil
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
	typ := core.MediaType(e.spec.MediaType)
	if typ != core.TypeMovie && typ != core.TypeShow {
		typ = core.TypeMovie
	}
	return core.Collection{
		Key: e.key, Title: e.spec.Name, Kind: "smart", Type: typ,
		Summary:     fmt.Sprintf("Generated from your search for %q. Rate it to keep it as your own collection.", e.descriptive),
		Sort:        core.SortOrder(e.spec.Sort),
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

// ephemeralPage reads the same AI cache as search. No model wait or catalogue
// scan occurs while a protocol browses an ephemeral collection.
func (s *Service) ephemeralPage(ctx context.Context, user *core.User, e *ephemeralEntry, offset, limit int) (core.Page, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	if e.descriptive != "" {
		result, err := s.DescribeSearch(ctx, user, e.descriptive, false)
		if err != nil {
			return core.Page{}, err
		}
		total := len(result.Items)
		if offset >= total {
			return core.Page{Items: []core.Title{}, Total: total}, nil
		}
		return core.Page{Items: result.Items[offset:pageEnd(offset, limit, total)], Total: total}, nil
	}
	return core.Page{}, core.ErrNotFound
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
	if len(page.Items) > 0 && e.spec.MediaType == MediaMixed {
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
		if t != "" && e.spec.MediaType != MediaMixed && core.MediaType(e.spec.MediaType) != t {
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
	spec := cloneSpec(e.spec)
	row, err := s.insertRow(ctx, spec, user.ID)

	if err != nil {
		return nil, false, err
	}
	s.vibeReg().update(key, func(live *ephemeralEntry) { live.promoted = row.key() })
	c := ephemeralCollection(e, e.childCount)
	c.Key = row.key()
	return &c, true, nil
}

var _ core.VibeSearcher = (*Service)(nil)
