package personalize

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/recommend"
)

// candItem is one title in a candidate row with its context-free score.
type candItem struct {
	t      core.Title
	base   float64 // generator score, roughly 0..1
	fit    float64 // taste fit, roughly -1..1
	qual   float64 // quality 0..1
	reason string
}

// candRow is a row a generator proposes. Assembly decides whether it is
// shown, where, and with which of its items.
type candRow struct {
	kind    string
	id      string // hub id stem without the section prefix: "top10", "because.123"
	title   string
	reason  string
	context string // Plex hub context stem
	source  string
	items   []candItem
	// minItems is the fewest items worth showing the row with (default 4).
	minItems int
	// maxItems caps the row regardless of the client's count (Top 10).
	maxItems int
	// keepOrder rows have a meaningful order (franchise release order,
	// recency) that ranking must not change.
	keepOrder bool
	// scale multiplies the row's predicted engagement (seed recency for
	// Because-you-watched rows).
	scale float64
	// collKey is a backing collection ("More" opens it).
	collKey core.RatingKey
	// typed holds type-native item lists for the Movies and TV Shows
	// pages (rows ranked over a mixed pool keep up to 40 of each type
	// there, instead of the mixed list's share). Where the row may appear
	// at all is its kind's surfaces (kinds.go, surfaces.go); owner
	// collection rows carry their own targeting.
	typed map[core.MediaType][]candItem
	// hub is set for rows that arrive as finished hubs (owner collections,
	// Continue Watching); assembly keeps their identity.
	hub *core.Hub
	// coldHome section rows also appear on new viewers' Homes; coldOnly
	// rows appear only for new viewers.
	coldHome, coldOnly bool
	// group: at most one row per group of the same kind on a screen
	// (showcase genre clusters).
	group string
	// sectionTitles name the row on the Movies and TV Shows pages when it
	// differs from its Home title (crowd rows).
	sectionTitles map[core.MediaType]string
}

// userModel is everything precomputed for one viewer: the taste model and
// the candidate rows. Assembly for a request context is pure CPU over it.
type userModel struct {
	user    core.User
	generic bool
	taste   *Taste
	rows    []*candRow
	fatigue map[core.RatingKey]float64 // item key -> 0..1
	// crowd and buzz are the build's crowd graph and trend momentum by
	// taste key (crowd.go, buzz.go): ranker features and Top 10 freshness.
	crowd   map[core.RatingKey]crowdFeat
	buzz    map[core.RatingKey]float64
	arms    map[string]arm // this viewer's per-kind row stats
	builtAt time.Time
	took    time.Duration
	// version identifies the build (memo keys).
	version             uint64
	preferenceRevision  uint64
	preferenceSignature string
}

// builder builds one userModel.
type builder struct {
	e       *Engine
	ctx     context.Context
	user    core.User
	generic bool
	taste   *Taste
	touched map[core.RatingKey]bool // taste keys with any history, status or play
	now     time.Time

	statuses map[core.RatingKey]core.WatchStatus
	memo     map[string][]core.Title
	fl       *floors
	// hydrateLeft is the remaining catalogue detail fetches of this build.
	hydrateLeft int
	fits        map[core.RatingKey]fitMemo
	rowsOut     []*candRow
	// crowd graph and trend momentum, computed on first use (crowd.go,
	// buzz.go).
	crowdDone bool
	crowdG    *crowdGraph
	buzzM     *buzzMap
}

type fitMemo struct {
	fit float64
	why string
}

// minPersonalConfidence is the taste confidence below which personal rows
// (Top 10, Because you watched…) are not generated: popularity rows do a
// better job for brand-new viewers.
const minPersonalConfidence = 0.08

// Confidence thresholds: Top 10 for You needs a real taste; below
// coldStartConfidence a viewer gets the curated cold-start Home (Top 10
// today, genre showcases, acclaimed and new titles) leading.
const (
	top10Confidence     = 0.25
	coldStartConfidence = 0.3
)

func (b *builder) personal() bool {
	return !b.generic && b.taste != nil && b.taste.Confidence >= minPersonalConfidence
}

// ---- catalogue access (memoised; failures degrade to empty) -----------------

func (b *builder) memoized(key string, fetch func() ([]core.Title, error)) []core.Title {
	if v, ok := b.memo[key]; ok {
		return v
	}
	items, err := fetch()
	if err != nil {
		b.e.log.Debug("personalize: candidate source failed", "source", key, "err", err)
		items = nil
	}
	b.memo[key] = items
	return items
}

func (b *builder) similar(k core.RatingKey, n int) []core.Title {
	return b.memoized(fmt.Sprintf("similar:%d:%d", k, n), func() ([]core.Title, error) {
		return b.e.catalog.Similar(b.ctx, k, n)
	})
}

func (b *builder) list(name string, t core.MediaType, n int) []core.Title {
	return b.memoized(fmt.Sprintf("list:%s:%s:%d", name, t, n), func() ([]core.Title, error) {
		return b.e.catalog.List(b.ctx, name, t, n)
	})
}

func (b *builder) browse(q core.BrowseQuery) []core.Title {
	if b.user.MaxContentRating != "" {
		q.Filter.MaxContentRating = b.user.MaxContentRating
	}
	return b.memoized(fmt.Sprintf("browse:%+v", q), func() ([]core.Title, error) {
		page, err := b.e.catalog.Browse(b.ctx, q)
		return page.Items, err
	})
}

func (b *builder) both(fetch func(t core.MediaType) []core.Title) []core.Title {
	return interleave(fetch(core.TypeMovie), fetch(core.TypeShow))
}

func interleave(a, c []core.Title) []core.Title {
	out := make([]core.Title, 0, len(a)+len(c))
	for i := 0; i < len(a) || i < len(c); i++ {
		if i < len(a) {
			out = append(out, a[i])
		}
		if i < len(c) {
			out = append(out, c[i])
		}
	}
	return out
}

// loadStatuses fetches watch statuses for keys not yet known.
func (b *builder) loadStatuses(items []core.Title) {
	if b.e.state == nil || b.generic {
		return
	}
	var need []core.RatingKey
	for _, t := range items {
		if _, ok := b.statuses[t.Key]; !ok {
			need = append(need, t.Key)
			b.statuses[t.Key] = core.WatchStatus{Key: t.Key}
		}
	}
	if len(need) == 0 {
		return
	}
	m, err := b.e.state.GetMany(b.ctx, b.user.ID, need)
	if err != nil {
		b.e.log.Debug("personalize: statuses failed", "err", err)
		return
	}
	for k, ws := range m {
		b.statuses[k] = ws
	}
}

// fresh reports whether a title may be recommended for discovery: a film
// or show, released, within the viewer's limits, never watched or started.
func (b *builder) fresh(t *core.Title) bool {
	if t.Type != core.TypeMovie && t.Type != core.TypeShow {
		return false
	}
	if !t.Released.IsZero() && t.Released.After(b.now) {
		return false
	}
	if !recommend.ContentRatingAllowed(t.ContentRating, b.user.MaxContentRating) {
		return false
	}
	if b.touched[tasteKey(t.Key)] {
		return false
	}
	ws := b.statuses[t.Key]
	return ws.ViewCount == 0 && ws.ViewOffset == 0
}

func (b *builder) fit(t *core.Title) (float64, string) {
	if m, ok := b.fits[t.Key]; ok {
		return m.fit, m.why
	}
	f, why := b.taste.Fit(t)
	b.fits[t.Key] = fitMemo{f, why}
	return f, why
}

// scorer turns a candidate (its index in the source list of n, its fit
// and the taste reason) into a score and an item reason.
type scorer func(t *core.Title, idx, n int, fit float64, why string) (float64, string)

// rank keeps fresh, unique, notable titles and scores them, best first.
func (b *builder) rank(items []core.Title, score scorer) []candItem {
	return b.rankWith(items, score, true)
}

// rankWith is rank, optionally keeping obscure titles (rows whose source
// already vouches for them: what other viewers here watched, Trakt).
func (b *builder) rankWith(items []core.Title, score scorer, requireNotable bool) []candItem {
	b.loadStatuses(items)
	seen := map[core.RatingKey]bool{}
	var out []candItem
	n := len(items)
	for i := range items {
		t := &items[i]
		if seen[t.Key] || !b.fresh(t) || requireNotable && !b.notable(t) {
			continue
		}
		seen[t.Key] = true
		fit, why := b.fit(t)
		s, reason := score(t, i, n, fit, why)
		if math.IsNaN(s) || math.IsInf(s, 0) {
			continue
		}
		out = append(out, candItem{t: *t, base: s, fit: fit, qual: quality(t), reason: reason})
	}
	sortItems(out)
	return out
}

func sortItems(items []candItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].base != items[j].base {
			return items[i].base > items[j].base
		}
		return items[i].t.Key < items[j].t.Key
	})
}

// capItems keeps the best n items, but keeps films and series both
// represented (at least n/3 each when available) so the Movies and TV
// screens, which filter a row by type, still have enough to show.
func capItems(items []candItem, n int) []candItem {
	if n <= 0 || len(items) <= n {
		return items
	}
	chosen := make([]bool, len(items))
	for i := 0; i < n; i++ {
		chosen[i] = true
	}
	isMovie := func(i int) bool { return items[i].t.Type == core.TypeMovie }
	for _, want := range []bool{true, false} {
		have := 0
		for i := 0; i < len(items); i++ {
			if chosen[i] && isMovie(i) == want {
				have++
			}
		}
		next := n
		for drop := len(items) - 1; have < n/3 && drop >= 0; drop-- {
			if !chosen[drop] || isMovie(drop) == want {
				continue
			}
			for next < len(items) && (chosen[next] || isMovie(next) != want) {
				next++
			}
			if next >= len(items) {
				break
			}
			chosen[drop], chosen[next] = false, true
			have++
		}
	}
	out := make([]candItem, 0, n)
	for i, ok := range chosen {
		if ok {
			out = append(out, items[i])
		}
	}
	return out
}

// splitTyped keeps the best n films and the best n series of a ranked
// list, for the Movies and TV Shows pages.
func splitTyped(items []candItem, n int) map[core.MediaType][]candItem {
	out := map[core.MediaType][]candItem{}
	for _, it := range items {
		t := baseType(it.t.Type)
		if (t == core.TypeMovie || t == core.TypeShow) && len(out[t]) < n {
			out[t] = append(out[t], it)
		}
	}
	return out
}

// withTyped sets a mixed row's type-native lists from its full ranking.
func withTyped(r *candRow, ranked []candItem) *candRow {
	r.typed = splitTyped(ranked, 40)
	return r
}

// typedPool is the per-type candidate pool for type-native lens rows:
// titles similar to recent favourites of this type, the type's trending,
// top rated and most popular titles, and the favourite genres' best of
// this type.
func (b *builder) typedPool(t core.MediaType) []core.Title {
	key := "pool:" + string(t)
	if v, ok := b.memo[key]; ok {
		return v
	}
	var all []core.Title
	for _, s := range b.seeds(10) {
		if baseType(s.Title.Type) != t {
			continue
		}
		for _, x := range b.similar(s.Key, 30) {
			if x.Type == t {
				all = append(all, x)
			}
		}
	}
	all = append(all, b.list("trending_week", t, 40)...)
	all = append(all, b.list("top_rated", t, 60)...)
	all = append(all, b.browse(core.BrowseQuery{Filter: core.Filter{Type: t}, Sort: core.SortPopularity, Desc: true, Limit: 120})...)
	for _, g := range b.taste.TopGenres(4) {
		all = append(all, b.browse(core.BrowseQuery{Filter: core.Filter{Type: t, Genres: []string{g}}, Sort: core.SortPopularity, Desc: true, Limit: 80})...)
		all = append(all, b.browse(core.BrowseQuery{Filter: core.Filter{Type: t, Genres: []string{g}, MinRating: 7}, Sort: core.SortRating, Desc: true, Limit: 40})...)
	}
	curDecade := b.now.Year() / 10 * 10
	for _, kv := range sortedByWeight(b.taste.Decade, 3) {
		if kv.w >= 0.4 && kv.k > 0 && kv.k < curDecade {
			all = append(all, b.browse(core.BrowseQuery{Filter: core.Filter{Type: t, YearFrom: kv.k, YearTo: kv.k + 9, MinRating: 7}, Sort: core.SortPopularity, Desc: true, Limit: 30})...)
		}
	}
	b.memo[key] = all
	return all
}

// ---- item features -------------------------------------------------------------

// popularity maps TMDB popularity onto 0..1 on a log scale.
func popularity(t *core.Title) float64 {
	return clamp(math.Log1p(math.Max(t.Popularity, 0))/math.Log1p(500), 0, 1)
}

// recency is 1 for today's releases, decaying with a ~4-month scale.
func recency(t *core.Title, now time.Time) float64 {
	if t.Released.IsZero() {
		return 0.3
	}
	days := now.Sub(t.Released).Hours() / 24
	if days < 0 {
		return 0
	}
	return math.Exp(-days / 120)
}

// rankScore is 1 for the first of n items, falling linearly to ~0.
func rankScore(idx, n int) float64 {
	if n <= 1 {
		return 1
	}
	return 1 - float64(idx)/float64(n)
}

func minutes(t *core.Title) float64 { return t.Runtime.Minutes() }
