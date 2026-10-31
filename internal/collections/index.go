package collections

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

// entry is the compact, in-memory form of one browsable movie or show:
// everything rule evaluation, sorting and title cards need, so a preview
// over the whole catalogue never touches SQLite.
type entry struct {
	key       core.RatingKey
	typ       core.MediaType
	title     string
	sortTitle string
	year      int
	released  int64 // unix seconds, 0 = unknown
	added     int64
	runtime   int // minutes, 0 = unknown
	rating    float64
	votes     int
	pop       float64
	lang      string
	crating   string
	collID    int64
	poster    string
	backdrop  string
	detail    bool

	genres      []string // display names
	genresLC    []string
	countriesLC []string
	studiosLC   []string
	keywordsLC  []string
	cast        []int64 // sorted
	directors   []int64
	writers     []int64
}

func (e *entry) hasPerson(rf ref) bool {
	if rf.ID <= 0 {
		return false
	}
	in := func(xs []int64) bool {
		i := sort.Search(len(xs), func(i int) bool { return xs[i] >= rf.ID })
		return i < len(xs) && xs[i] == rf.ID
	}
	switch rf.Role {
	case "cast":
		return in(e.cast)
	case "director":
		return in(e.directors)
	case "writer":
		return in(e.writers)
	case "crew":
		return in(e.directors) || in(e.writers)
	}
	return in(e.cast) || in(e.directors) || in(e.writers)
}

// card converts an entry to a light core.Title for previews.
func (e *entry) card() core.Title {
	t := core.Title{
		Key: e.key, Type: e.typ, Title: e.title, Year: e.year, Rating: e.rating, VoteCount: e.votes,
		Popularity: e.pop, ContentRating: e.crating, PosterPath: e.poster, BackdropPath: e.backdrop,
		Runtime: time.Duration(e.runtime) * time.Minute, Language: e.lang, Genres: e.genres,
		CollectionID: e.collID, Countries: e.countriesLC, Studios: e.studiosLC, Keywords: e.keywordsLC,
	}
	if e.released != 0 {
		t.Released = time.Unix(e.released, 0).UTC()
	}
	switch e.typ {
	case core.TypeMovie:
		t.IDs.TMDB = e.key.TMDB()
	case core.TypeShow:
		t.IDs.TMDB = e.key.TMDB()
	}
	return t
}

// entryFromTitle builds an entry from a full title (TMDB results joined to
// the catalogue, or titles loaded from the mirror).
func entryFromTitle(t core.Title, in *interner) *entry {
	e := &entry{
		key: t.Key, typ: t.Type, title: t.Title, sortTitle: t.SortTitle, year: t.Year,
		runtime: int(t.Runtime / time.Minute), rating: t.Rating, votes: t.VoteCount, pop: t.Popularity,
		lang: in.lower(t.Language), crating: in.str(t.ContentRating), collID: t.CollectionID,
		poster: t.PosterPath, backdrop: t.BackdropPath,
	}
	if e.sortTitle == "" {
		e.sortTitle = sortKey(t.Title)
	}
	if e.year == 0 && !t.Released.IsZero() {
		e.year = t.Released.Year()
	}
	if !t.Released.IsZero() {
		e.released = t.Released.Unix()
	}
	for _, g := range t.Genres {
		e.genres = append(e.genres, in.str(g))
		e.genresLC = append(e.genresLC, in.lower(g))
	}
	for _, c := range t.Countries {
		e.countriesLC = append(e.countriesLC, in.lower(c))
	}
	for _, s := range t.Studios {
		e.studiosLC = append(e.studiosLC, in.lower(s))
	}
	for _, k := range t.Keywords {
		e.keywordsLC = append(e.keywordsLC, in.lower(k))
	}
	for _, p := range t.Cast {
		e.cast = append(e.cast, p.TMDB)
	}
	for _, p := range t.Directors {
		e.directors = append(e.directors, p.TMDB)
	}
	for _, p := range t.Writers {
		e.writers = append(e.writers, p.TMDB)
	}
	e.detail = len(t.Keywords) > 0 || len(t.Cast) > 0 || t.Runtime > 0
	e.finish()
	return e
}

func (e *entry) finish() {
	for _, xs := range [][]int64{e.cast, e.directors, e.writers} {
		sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	}
}

func sortKey(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	for _, p := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(l, p) {
			return strings.TrimSpace(l[len(p):])
		}
	}
	return l
}

// interner shares the many repeated strings (genres, studios, languages).
type interner struct{ m map[string]string }

func newInterner() *interner { return &interner{m: map[string]string{}} }

func (in *interner) str(s string) string {
	if s == "" {
		return ""
	}
	if v, ok := in.m[s]; ok {
		return v
	}
	in.m[s] = s
	return s
}

func (in *interner) lower(s string) string { return in.str(strings.ToLower(strings.TrimSpace(s))) }

// index is a snapshot of the browsable catalogue, rebuilt in the background
// when the mirror changes. Readers get the current snapshot without
// blocking; the first load is synchronous.
type index struct {
	mirror   *catalog.Mirror
	now      func() time.Time
	checkTTL time.Duration

	mu        sync.RWMutex
	snap      *snapshot
	checkedAt time.Time
	building  bool
	buildDone chan struct{}
	extra     map[core.RatingKey]*entry // joined since the last rebuild
	intern    *interner
	// gen is the catalogue's write generation, when it has one.
	gen        func() uint64
	vouchedGen uint64    // gen at the last build or matching fingerprint
	vouchedAt  time.Time // when vouchedGen was taken
	nextBuild  time.Time // the earliest background rebuild
	// beforeLoad, when set (tests), runs before a build reads the mirror.
	beforeLoad func()
}

type snapshot struct {
	facetsOnce sync.Once
	facets     map[string][]core.RatingKey
	entries    map[core.RatingKey]*entry
	order      []*entry // popularity desc, stable
	print      fingerprint
	builtAt    time.Time
}

type fingerprint struct {
	count             int
	maxFetch, maxAdd  int64
	credits, keywords int
}

func newIndex(db *sql.DB, now func() time.Time, checkTTL time.Duration) *index {
	return &index{mirror: catalog.NewMirror(db, now), now: now, checkTTL: checkTTL, extra: map[core.RatingKey]*entry{}, intern: newInterner()}
}

// get returns the current snapshot, building it on first use and
// refreshing it in the background when the mirror has changed.
func (ix *index) get(ctx context.Context) (*snapshot, error) {
	ix.mu.RLock()
	snap := ix.snap
	ix.mu.RUnlock()
	if snap == nil {
		return ix.rebuild(ctx)
	}
	if ix.checkTTL <= 0 {
		// Tests: always fresh.
		if fp, err := ix.fingerprint(ctx); err == nil && fp != snap.print {
			return ix.rebuild(ctx)
		}
		return ix.current(), nil
	}
	ix.checkBehind()
	return ix.current(), nil
}

// ready returns the current snapshot without waiting. Before the first
// build it starts one in the background and returns nil. The snapshot and
// its fingerprint both read the whole catalogue, so neither runs on a
// request that can do without it.
func (ix *index) ready() *snapshot {
	snap := ix.current()
	if snap == nil {
		ix.buildBehind()
		return nil
	}
	if ix.checkTTL > 0 {
		ix.checkBehind()
	}
	return snap
}

// readyWithin is ready, waiting up to d for a first build.
func (ix *index) readyWithin(ctx context.Context, d time.Duration) *snapshot {
	if snap := ix.current(); snap != nil {
		if ix.checkTTL > 0 {
			ix.checkBehind()
		}
		return snap
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		bctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = ix.rebuild(bctx)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return ix.current()
}

// fingerprintEvery bounds how long an unchanged write generation alone
// vouches for the snapshot: writes from another cluster node do not move it.
const fingerprintEvery = 10 * time.Minute

// checkBehind looks for catalogue changes once per checkTTL, off the
// request, and rebuilds when there are any. An unchanged write generation
// skips the fingerprint, which counts the whole catalogue. A rebuild reads
// the whole catalogue too, so rebuilds are spaced by ten times the last
// one's duration.
func (ix *index) checkBehind() {
	ix.mu.Lock()
	now := ix.now()
	if ix.building || now.Sub(ix.checkedAt) < ix.checkTTL || now.Before(ix.nextBuild) {
		ix.mu.Unlock()
		return
	}
	ix.checkedAt = now
	print := ix.snap.print
	var gen uint64
	if ix.gen != nil {
		gen = ix.gen()
		if gen == ix.vouchedGen && now.Sub(ix.vouchedAt) < fingerprintEvery {
			ix.mu.Unlock()
			return
		}
	}
	ix.mu.Unlock()
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		fp, err := ix.fingerprint(bctx)
		if err != nil {
			return
		}
		if fp == print {
			ix.mu.Lock()
			ix.vouchedGen, ix.vouchedAt = gen, ix.now()
			ix.mu.Unlock()
			return
		}
		_, _ = ix.rebuild(bctx)
	}()
}

// buildBehind starts the first build in the background.
func (ix *index) buildBehind() {
	ix.mu.RLock()
	building := ix.building
	ix.mu.RUnlock()
	if building {
		return
	}
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = ix.rebuild(bctx)
	}()
}

// current returns the snapshot (which already includes joined entries).
func (ix *index) current() *snapshot {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.snap
}

// add records titles joined to the browsable set since the last rebuild.
// The snapshot is copied on write so readers never see a partial update.
func (ix *index) add(ts []core.Title) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var fresh []*entry
	for _, t := range ts {
		if t.Type != core.TypeMovie && t.Type != core.TypeShow {
			continue
		}
		e := entryFromTitle(t, ix.intern)
		ix.extra[t.Key] = e
		fresh = append(fresh, e)
	}
	if len(fresh) == 0 || ix.snap == nil {
		return
	}
	ix.snap = mergeEntries(ix.snap, fresh)
}

func mergeEntries(base *snapshot, add []*entry) *snapshot {
	out := &snapshot{entries: make(map[core.RatingKey]*entry, len(base.entries)+len(add)),
		order: make([]*entry, 0, len(base.order)+len(add)), print: base.print, builtAt: base.builtAt}
	for k, e := range base.entries {
		out.entries[k] = e
	}
	replaced := map[core.RatingKey]*entry{}
	for _, e := range add {
		if _, ok := out.entries[e.key]; ok {
			replaced[e.key] = e
		}
		out.entries[e.key] = e
	}
	for _, e := range base.order {
		if r, ok := replaced[e.key]; ok {
			out.order = append(out.order, r)
			continue
		}
		out.order = append(out.order, e)
	}
	for _, e := range add {
		if _, ok := replaced[e.key]; !ok {
			out.order = append(out.order, e)
		}
	}
	return out
}

// invalidate forces the next get to check the fingerprint.
func (ix *index) invalidate() {
	ix.mu.Lock()
	ix.checkedAt = time.Time{}
	ix.mu.Unlock()
}

func (ix *index) fingerprint(ctx context.Context) (fingerprint, error) {
	fp, err := ix.mirror.Fingerprint(ctx)
	return fingerprint{count: fp.Count, maxFetch: fp.MaxFetch, maxAdd: fp.MaxAdd}, err
}

func (ix *index) rebuild(ctx context.Context) (*snapshot, error) {
	ix.mu.Lock()
	if ix.building {
		done := ix.buildDone
		ix.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if s := ix.current(); s != nil {
			return s, nil
		}
		return ix.rebuild(ctx)
	}
	ix.building = true
	ix.buildDone = make(chan struct{})
	started := ix.now()
	var gen uint64
	if ix.gen != nil {
		gen = ix.gen()
	}
	ix.mu.Unlock()

	snap, err := ix.load(ctx)

	ix.mu.Lock()
	ix.building = false
	close(ix.buildDone)
	if err == nil {
		ix.vouchedGen, ix.vouchedAt = gen, started
		ix.nextBuild = ix.now().Add(10 * ix.now().Sub(started))
		// Keep joined entries the new snapshot does not have yet.
		var keep []*entry
		for k, e := range ix.extra {
			if _, ok := snap.entries[k]; ok {
				delete(ix.extra, k)
				continue
			}
			keep = append(keep, e)
		}
		if len(keep) > 0 {
			snap = mergeEntries(snap, keep)
		}
		ix.snap = snap
		ix.checkedAt = started
	}
	ix.mu.Unlock()
	if err != nil {
		if s := ix.current(); s != nil {
			return s, nil
		}
		return nil, err
	}
	return ix.current(), nil
}

func (ix *index) load(ctx context.Context) (*snapshot, error) {
	if ix.beforeLoad != nil {
		ix.beforeLoad()
	}
	fp, err := ix.fingerprint(ctx)
	if err != nil {
		return nil, err
	}
	items, err := ix.mirror.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	in := newInterner()
	snap := &snapshot{entries: make(map[core.RatingKey]*entry, len(items)), print: fp, builtAt: ix.now()}
	for _, item := range items {
		e := entryFromTitle(item.Title, in)
		if added := item.Title.DateAdded(ix.now()); !added.IsZero() {
			e.added = added.Unix()
		}
		e.detail = e.detail || item.Detailed
		snap.entries[e.key] = e
		snap.order = append(snap.order, e)
	}
	return snap, nil
}

// lookupRows loads entries for keys that are not in the snapshot (titles
// in the mirror that are not browsable yet).
func (ix *index) lookupRows(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]*entry, error) {
	out := map[core.RatingKey]*entry{}
	if len(keys) == 0 {
		return out, nil
	}
	titles, err := ix.mirror.Titles(ctx, keys)
	if err != nil {
		return nil, err
	}
	in := newInterner()
	for _, t := range titles {
		out[t.Key] = entryFromTitle(t, in)
	}
	return out, nil
}
