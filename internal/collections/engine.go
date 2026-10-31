package collections

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

// maxMembers caps how many titles a collection keeps (before its limit).
const maxMembers = 5000

// buildResult is one evaluation of a spec.
type buildResult struct {
	keys      []core.RatingKey // sorted members, before the spec's limit
	total     int              // matches (may exceed len(keys) when capped)
	warnings  []string
	snap      *snapshot
	personal  []*rule // per-user rules, applied when serving
	personalX []*rule // per-user exclusions, applied when serving
	// unshared (only for match "any" with per-user rules) lists members that
	// met none of the shared rules; they need a per-user rule to hold.
	unshared  map[core.RatingKey]bool
	spec      core.CollectionSpec
	builtAt   time.Time
	hash      string
	resolved  core.CollectionSpec // spec with labels resolved to ids
	hasDetail bool
}

// itemCount is what the owner sees: matches capped by the limit.
func (b *buildResult) itemCount() int {
	n := b.total
	if b.spec.Limit > 0 && n > b.spec.Limit {
		n = b.spec.Limit
	}
	return n
}

type evalOptions struct {
	seed string // stable id for random sort
	// persist joins external titles to the catalogue (builds); previews
	// only overlay them.
	persist bool
	prior   []core.RatingKey
}

// cand is a candidate title with the rules a source already guarantees.
type cand struct {
	e       *entry
	order   int
	airDate int64 // selected TV date, 0 when unknown
	assured map[*rule]bool
}

// evaluate runs a spec end to end: gather, join, filter, sort.
func (s *Service) evaluate(ctx context.Context, spIn core.CollectionSpec, opt evalOptions) (*buildResult, error) {
	sp := cloneSpec(spIn)
	Normalize(&sp)
	if err := Validate(&sp); err != nil {
		return nil, err
	}
	rules, err := compileRules(sp.Rules)
	if err != nil {
		return nil, err
	}
	excludes, err := compileRules(sp.Exclude)
	if err != nil {
		return nil, err
	}
	res := &buildResult{spec: sp, builtAt: s.now()}
	warn := func(format string, args ...any) { res.warnings = append(res.warnings, fmt.Sprintf(format, args...)) }

	// Resolve names <-> ids in rules (keyword names for local matching,
	// ids for TMDB pushdown).
	if !sp.LocalOnly {
		s.resolveRuleRefs(ctx, rules, warn)
		s.resolveRuleRefs(ctx, excludes, warn)
	}
	// Per-user rules (watched, in progress) are applied when serving each
	// viewer; the build works on everything else.
	rules = splitPersonal(rules, &res.personal)
	excludes = splitPersonal(excludes, &res.personalX)

	snap, err := s.idx.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("collections: catalogue index: %w", err)
	}

	// Gather every source concurrently.
	results := make([]sourceResult, len(sp.Sources))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, src := range sp.Sources {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, src core.CollectionSource) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = s.gather(ctx, &sp, src, rules)
		}(i, src)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res.resolved = cloneSpec(sp)
	for i, r := range results {
		res.warnings = append(res.warnings, r.warnings...)
		if r.resolvedID > 0 && res.resolved.Sources[i].ID <= 0 {
			res.resolved.Sources[i].ID = r.resolvedID
		}
		if r.resolvedLabel != "" && res.resolved.Sources[i].Label == "" {
			res.resolved.Sources[i].Label = r.resolvedLabel
		}
	}
	hasCatalog := false
	for _, r := range results {
		if r.all {
			hasCatalog = true
		}
	}

	// Push rules the mirror only partly knows (keywords, people, runtime…)
	// down to TMDB discover so the whole catalogue is searched for them.
	assured := map[core.RatingKey]map[*rule]bool{}
	var pushed []core.Title
	if hasCatalog && s.api.c != nil && !sp.LocalOnly {
		pushed, assured = s.pushdown(ctx, &sp, rules, warn)
	} else if hasCatalog && !sp.LocalOnly {
		for _, r := range rules {
			if r.detailOnly() && r.field != "watchProvider" {
				warn("%s is only known for titles whose details were fetched; connect TMDB for complete results", r.field)
				break
			}
		}
	}

	// Join external titles to the browsable catalogue.
	var external []core.Title
	for _, r := range results {
		external = append(external, r.titles...)
	}
	external = append(external, pushed...)
	var keyOnly []core.RatingKey
	for _, r := range results {
		for _, k := range r.keys {
			if _, ok := snap.entries[k]; !ok {
				keyOnly = append(keyOnly, k)
			}
		}
	}
	if len(keyOnly) > 0 {
		external = append(external, s.titlesFor(ctx, keyOnly)...)
	}
	snap, err = s.join(ctx, snap, external, opt.persist)
	if err != nil {
		return nil, err
	}

	providerTitles := []core.Title{}
	// Watch-provider rules need the provider's catalogue from TMDB.
	ec := &evalCtx{now: s.now(), providers: map[*rule]map[core.RatingKey]bool{}}
	for _, r := range append(append([]*rule{}, rules...), excludes...) {
		if r.field == "watchProvider" {
			set, titles := s.providerSet(ctx, &sp, r, warn)
			ec.providers[r] = set
			providerTitles = append(providerTitles, titles...)
			snap, err = s.join(ctx, snap, titles, opt.persist)
			if err != nil {
				return nil, err
			}
		}
	}

	if sp.ReleaseMode == "upcoming" || sp.ReleaseMode == "mixed" {
		candidates := map[core.RatingKey]bool{}
		for _, e := range snap.order {
			if e.released > ec.now.Unix() {
				candidates[e.key] = true
			}
		}
		for _, t := range append(external, providerTitles...) {
			if !t.Released.IsZero() && t.Released.After(ec.now) {
				candidates[t.Key] = true
			}
		}
		for _, k := range opt.prior {
			candidates[k] = true
		}
		keys := make([]core.RatingKey, 0, len(candidates))
		for k := range candidates {
			if k.Kind() == core.KindMovie || k.Kind() == core.KindShow {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		refresher, ok := s.cat.(core.AuthoritativeTitleRefresher)
		entries := make([]*entry, 0, len(keys))
		in := newInterner()
		failures := 0
		refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		for i, k := range keys {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var t *core.Title
			var err error
			if !ok || i >= 500 || refreshCtx.Err() != nil {
				err = errors.New("refresh unavailable or budget exhausted")
			} else {
				t, err = refresher.RefreshTitle(refreshCtx, k, opt.persist)
			}
			if err != nil || t == nil || t.Released.IsZero() {
				failures++
				if old := snap.entries[k]; old != nil {
					copy := *old
					copy.released = 0
					entries = append(entries, &copy)
				}
				continue
			}
			entries = append(entries, entryFromTitle(*t, in))
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if failures > 0 {
			warn("Upcoming dates could not be refreshed for %d titles; they were excluded", failures)
		}
		snap = mergeEntries(snap, entries)
	}

	// Union candidates in source order.
	cands := make([]*cand, 0, 256)
	seen := map[core.RatingKey]*cand{}
	addKey := func(k core.RatingKey) {
		if c := seen[k]; c != nil {
			return
		}
		e := snap.entries[k]
		if e == nil {
			return
		}
		c := &cand{e: e, order: len(cands)}
		seen[k] = c
		cands = append(cands, c)
	}
	for _, r := range results {
		switch {
		case r.all:
			for _, e := range snap.order {
				addKey(e.key)
			}
			for _, t := range r.titles {
				addKey(t.Key)
			}
			// Pushdown matches come right after the catalogue they extend.
			for _, t := range pushed {
				addKey(t.Key)
			}
		default:
			for _, t := range r.titles {
				addKey(t.Key)
			}
			for _, k := range r.keys {
				addKey(k)
			}
		}
		for k, rs := range r.assured {
			m := assured[k]
			if m == nil {
				m = map[*rule]bool{}
				assured[k] = m
			}
			for _, x := range rs {
				m[x] = true
			}
		}
	}
	for k, as := range assured {
		if c := seen[k]; c != nil {
			c.assured = as
		}
	}
	// Release dates are mandatory, including explicit upcoming sources.
	nowUnix := ec.now.Unix()
	// With match "any", per-user rules are alternatives to the shared ones:
	// a title failing every shared rule may still match for some viewer, so
	// the build keeps it and serving decides.
	anyPersonal := sp.Match == MatchAny && len(res.personal) > 0

	// Filter.
	want := func(e *entry) bool {
		switch sp.MediaType {
		case MediaMovie:
			return e.typ == core.TypeMovie
		case MediaShow:
			return e.typ == core.TypeShow
		}
		return e.typ == core.TypeMovie || e.typ == core.TypeShow
	}
	var kept []*cand
	for _, c := range cands {
		if !want(c.e) {
			continue
		}
		if c.e.released == 0 || (sp.ReleaseMode == "upcoming" && c.e.released <= nowUnix) ||
			(c.e.released > nowUnix && (sp.ReleaseMode != "upcoming" && sp.ReleaseMode != "mixed" || c.e.released > ec.now.AddDate(0, 0, sp.LookaheadDays).Unix())) {
			continue
		}
		shared := true
		if anyPersonal {
			shared = len(rules) > 0 && matchesRules(c, rules, sp.Match, ec)
		} else if !matchesRules(c, rules, sp.Match, ec) {
			continue
		}
		excluded := false
		for _, x := range excludes {
			if x.match(c.e, ec) {
				excluded = true
				break
			}
		}
		if !excluded {
			kept = append(kept, c)
			if !shared {
				if res.unshared == nil {
					res.unshared = map[core.RatingKey]bool{}
				}
				res.unshared[c.e.key] = true
			}
		}
	}

	if sp.Sort == "latestEpisode" || sp.Sort == "latestSeasonPremiere" {
		unknown, stale, err := s.hydrateAirDates(ctx, kept, sp.Sort, ec.now, !sp.LocalOnly)
		if err != nil {
			return nil, fmt.Errorf("collections: air dates: %w", err)
		}
		if stale > 0 {
			warn("Air dates may be out of date for %d shows; they will refresh as the collection updates", stale)
		}
		if unknown > 0 {
			warn("Air dates are still loading or unavailable for %d shows; shows without dates appear last", unknown)
		}
	}
	sortCands(kept, sp.Sort, sp.SortDesc, randomSeed(opt.seed, sp, ec.now))
	res.total = len(kept)
	if anyPersonal {
		// Unshared members are only candidates for per-viewer rules: the
		// count and the cap belong to titles meeting a shared rule, which
		// keep their places ahead of the candidates (each group in sort
		// order) so a large catalogue cannot crowd real matches out.
		res.total = len(kept) - len(res.unshared)
		kept = sharedFirst(kept, res.unshared, maxMembers)
	}
	if len(kept) > maxMembers {
		kept = kept[:maxMembers]
		if sp.Limit == 0 && res.total > maxMembers {
			warn("%d titles match; only the first %d are kept", res.total, maxMembers)
		}
	}
	res.keys = make([]core.RatingKey, len(kept))
	for i, c := range kept {
		res.keys[i] = c.e.key
	}
	res.snap = snap
	if len(kept) == 0 {
		warn("no titles match; loosen the rules or add a source")
	}
	res.hash = buildHash(sp, res.keys, res.unshared)
	return res, nil
}

// sharedFirst bounds an any-match build with per-viewer rules to limit
// members: titles meeting a shared rule first, then the per-viewer
// candidates, each group in its sorted order. Candidates cut off are
// forgotten from unshared.
func sharedFirst(kept []*cand, unshared map[core.RatingKey]bool, limit int) []*cand {
	if len(kept) <= limit {
		return kept
	}
	ordered := make([]*cand, 0, len(kept))
	for _, c := range kept {
		if !unshared[c.e.key] {
			ordered = append(ordered, c)
		}
	}
	for _, c := range kept {
		if unshared[c.e.key] {
			ordered = append(ordered, c)
		}
	}
	for _, c := range ordered[limit:] {
		delete(unshared, c.e.key)
	}
	return ordered
}

func splitPersonal(rs []*rule, personal *[]*rule) []*rule {
	out := rs[:0:0]
	for _, r := range rs {
		if r.personal() {
			*personal = append(*personal, r)
			continue
		}
		out = append(out, r)
	}
	return out
}

func matchesRules(c *cand, rules []*rule, match string, ec *evalCtx) bool {
	if len(rules) == 0 {
		return true
	}
	if match == MatchAny {
		for _, r := range rules {
			if c.assured[r] || r.match(c.e, ec) {
				return true
			}
		}
		return false
	}
	for _, r := range rules {
		if !c.assured[r] && !r.match(c.e, ec) {
			return false
		}
	}
	return true
}

func randomSeed(seed string, sp core.CollectionSpec, now time.Time) uint64 {
	h := fnv.New64a()
	h.Write([]byte(seed))
	h.Write([]byte(sp.Name))
	h.Write([]byte(now.Format("2006-01-02")))
	return h.Sum64()
}

func (s *Service) loadAirDates(ctx context.Context, cs []*cand, mode string, now time.Time) error {
	dates, err := catalog.NewMirror(s.db, nil).AirDates(ctx, now, mode == "latestSeasonPremiere")
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.e.typ == core.TypeShow {
			c.airDate = dates[c.e.key]
		}
	}
	return nil
}

func sortCands(cs []*cand, mode string, desc bool, seed uint64) {
	if mode == "random" {
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].e.key < cs[j].e.key })
		r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		r.Shuffle(len(cs), func(i, j int) { cs[i], cs[j] = cs[j], cs[i] })
		return
	}
	less := func(a, b *cand) (bool, bool) { // (less, equal)
		switch mode {
		case "rating":
			if a.e.rating != b.e.rating {
				return a.e.rating < b.e.rating, false
			}
			return a.e.votes < b.e.votes, a.e.votes == b.e.votes
		case "latestEpisode", "latestSeasonPremiere":
			return a.airDate < b.airDate, a.airDate == b.airDate
		case "released":
			return a.e.released < b.e.released, a.e.released == b.e.released
		case "title":
			return a.e.sortTitle < b.e.sortTitle, a.e.sortTitle == b.e.sortTitle
		case "added":
			return a.e.added < b.e.added, a.e.added == b.e.added
		case "source":
			return a.order < b.order, a.order == b.order
		default: // popularity, personal (re-ranked per user when served)
			return a.e.pop < b.e.pop, a.e.pop == b.e.pop
		}
	}
	if mode == "personal" {
		desc = true
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if mode == "latestEpisode" || mode == "latestSeasonPremiere" {
			if (cs[i].airDate == 0) != (cs[j].airDate == 0) {
				return cs[i].airDate != 0
			}
		}
		l, eq := less(cs[i], cs[j])
		if eq {
			// Ties: popularity, then key, for a stable result.
			if cs[i].e.pop != cs[j].e.pop {
				return cs[i].e.pop > cs[j].e.pop
			}
			return cs[i].e.key < cs[j].e.key
		}
		if desc {
			return !l
		}
		return l
	})
}

func buildHash(sp core.CollectionSpec, keys []core.RatingKey, unshared map[core.RatingKey]bool) string {
	h := sha1.New()
	js, _ := specJSON(sp)
	h.Write([]byte(js))
	for _, k := range keys {
		h.Write([]byte(strconv.FormatInt(int64(k), 10)))
		if unshared[k] {
			h.Write([]byte{'?'})
		}
		h.Write([]byte{','})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// join makes TMDB titles part of the browsable catalogue (as a search
// does) so Plex can browse and play every member, and adds them to the
// index. Previews (persist=false) only overlay them on the snapshot, so
// trying out a source never changes the library. It returns the snapshot
// to evaluate against.
func (s *Service) join(ctx context.Context, snap *snapshot, titles []core.Title, persist bool) (*snapshot, error) {
	var fresh []core.Title
	seen := map[core.RatingKey]bool{}
	for _, t := range titles {
		if t.Key == 0 || seen[t.Key] || (t.Type != core.TypeMovie && t.Type != core.TypeShow) {
			continue
		}
		seen[t.Key] = true
		if _, ok := snap.entries[t.Key]; ok {
			continue
		}
		fresh = append(fresh, t)
	}
	if len(fresh) == 0 {
		return snap, nil
	}
	if !persist {
		keys := make([]core.RatingKey, len(fresh))
		for i, t := range fresh {
			keys[i] = t.Key
		}
		known := map[core.RatingKey]core.Title{}
		if loaded, err := loadCollectionTitles(ctx, s.db, keys); err == nil {
			for _, t := range loaded {
				if len(t.Genres) > 0 {
					known[t.Key] = t
				}
			}
		}
		in := newInterner()
		entries := make([]*entry, 0, len(fresh))
		for _, t := range fresh {
			if k, ok := known[t.Key]; ok {
				t = k
			}
			entries = append(entries, entryFromTitle(t, in))
		}
		return mergeEntries(snap, entries), nil
	}
	if s.upserter == nil {
		for _, t := range fresh {
			if !t.Released.IsZero() && t.Released.After(s.now()) {
				return nil, fmt.Errorf("collections: cannot store upcoming title %s", t.Key)
			}
		}
	}
	if s.upserter != nil {
		for _, t := range fresh {
			if err := s.upserter.UpsertTitle(ctx, t, t.IsReleased(s.now())); err != nil {
				return nil, fmt.Errorf("collections: store title %s: %w", t.Key, err)
			}
		}
		// Reload so entries carry what the mirror knows (details fetched
		// earlier, content ratings) on top of the summary.
		keys := make([]core.RatingKey, len(fresh))
		for i, t := range fresh {
			keys[i] = t.Key
		}
		loaded, err := loadCollectionTitles(ctx, s.db, keys)
		if err != nil {
			return nil, fmt.Errorf("collections: reload titles: %w", err)
		}
		if len(loaded) > 0 {
			fresh = loaded
		}
	}
	var released, future []core.Title
	for _, t := range fresh {
		if t.IsReleased(s.now()) {
			released = append(released, t)
		} else {
			future = append(future, t)
		}
	}
	s.idx.add(released)
	if len(fresh) > 0 {
		s.notify("index")
	}
	if cur := s.idx.current(); cur != nil {
		// Keep evaluation-only entries while preferring the latest index values.
		// The index excludes future titles, which may have been overlaid by
		// an earlier join in this same evaluation.
		snap = mergeEntries(snap, cur.order)
	}
	in := newInterner()
	entries := make([]*entry, 0, len(future))
	for _, t := range future {
		entries = append(entries, entryFromTitle(t, in))
	}
	return mergeEntries(snap, entries), nil
}

// resolveRuleRefs completes rule refs: names for local matching, ids for
// pushdown.
func (s *Service) resolveRuleRefs(ctx context.Context, rules []*rule, warn func(string, ...any)) {
	for _, r := range rules {
		if r.class != classRefs {
			continue
		}
		kind := map[string]string{"keyword": "keyword", "person": "person", "studio": "company",
			"network": "network", "collection": "collection", "watchProvider": "watchProvider"}[r.field]
		region := s.region(r.region)
		for i, rf := range r.refs {
			if rf.ID > 0 && rf.Name != "" {
				continue
			}
			// Local matching needs ids for people/collections and names
			// for keywords/studios/networks.
			if s.api.c == nil {
				needs := (r.field == "person" || r.field == "collection" || r.field == "watchProvider") && rf.ID <= 0
				needs = needs || ((r.field == "keyword" || r.field == "studio" || r.field == "network") && rf.Name == "")
				if needs {
					warn("%s %q can't be looked up without TMDB", r.field, firstNonEmpty(rf.Name, strconv.FormatInt(rf.ID, 10)))
				}
				continue
			}
			got, err := s.names.resolve(ctx, kind, ref{ID: rf.ID, Name: rf.Name}, region)
			if err != nil {
				warn("couldn't find the %s %q on TMDB", r.field, firstNonEmpty(rf.Name, strconv.FormatInt(rf.ID, 10)))
				continue
			}
			got.Role = rf.Role
			r.refs[i] = got
		}
	}
}

// pushdown queries TMDB discover for the rules the mirror only partly
// knows. It returns the titles found and, per title, the rules the query
// guarantees.
func (s *Service) pushdown(ctx context.Context, sp *core.CollectionSpec, rules []*rule, warn func(string, ...any)) ([]core.Title, map[core.RatingKey]map[*rule]bool) {
	var detail []*rule
	for _, r := range rules {
		if r.detailOnly() && !r.personal() {
			detail = append(detail, r)
		}
	}
	assured := map[core.RatingKey]map[*rule]bool{}
	if len(detail) == 0 {
		return nil, assured
	}
	want := wantFor(sp)
	var out []core.Title
	run := func(kind string, q url.Values, covers []*rule) {
		if q.Get("sort_by") == "" {
			q.Set("sort_by", discoverSort(sp.Sort, kind))
		}
		items, _, err := s.api.discover(ctx, kind, q, want, pagesFor(want))
		if err != nil {
			warn("TMDB discover: %v", err)
			var partial *partialListError
			if !errors.As(err, &partial) {
				return
			}
		}
		for _, t := range items {
			m := assured[t.Key]
			if m == nil {
				m = map[*rule]bool{}
				assured[t.Key] = m
			}
			for _, r := range covers {
				m[r] = true
			}
			out = append(out, t)
		}
	}
	for _, kind := range kindsFor(sp.MediaType) {
		if sp.Match == MatchAny {
			for _, r := range detail {
				q := url.Values{}
				if s.translate(ctx, r, kind, q) {
					run(kind, q, []*rule{r})
				}
			}
			continue
		}
		q := url.Values{}
		var covers []*rule
		ok := true
		for _, r := range rules {
			if r.personal() {
				continue
			}
			extra := url.Values{}
			switch {
			case !s.translate(ctx, r, kind, extra):
				if r.detailOnly() {
					ok = false // TMDB can't express it for this kind
				}
			case mergeParams(q, extra):
				covers = append(covers, r)
			}
		}
		if ok && len(covers) > 0 {
			run(kind, q, covers)
		}
	}
	return out, assured
}

func discoverSort(mode, kind string) string {
	switch mode {
	case "rating":
		return "vote_average.desc"
	case "released":
		if kind == "tv" {
			return "first_air_date.desc"
		}
		return "primary_release_date.desc"
	}
	return "popularity.desc"
}

// translate adds a rule's TMDB discover equivalent to q. It reports false
// when discover cannot express the rule exactly for kind.
func (s *Service) translate(ctx context.Context, r *rule, kind string, q url.Values) bool {
	join := func(op string) string {
		if op == "in" {
			return "|"
		}
		return ","
	}
	ids := func() ([]string, bool) {
		var out []string
		for _, rf := range r.refs {
			if rf.ID <= 0 {
				return nil, false
			}
			out = append(out, strconv.FormatInt(rf.ID, 10))
		}
		return out, len(out) > 0
	}
	dateKey := func(bound string) string {
		if kind == "tv" {
			return "first_air_date." + bound
		}
		return "primary_release_date." + bound
	}
	setRange := func(lo, hi string) {
		if lo != "" {
			q.Set(dateKey("gte"), lo)
		}
		if hi != "" {
			q.Set(dateKey("lte"), hi)
		}
	}
	switch r.field {
	case "genre":
		var gids []string
		for _, g := range r.tags {
			id, ok := s.api.genreID(ctx, kind, g)
			if !ok {
				return false
			}
			gids = append(gids, strconv.Itoa(id))
		}
		switch r.op {
		case "is", "in":
			q.Set("with_genres", strings.Join(gids, join(r.op)))
		default:
			q.Set("without_genres", strings.Join(gids, ","))
		}
		return true
	case "keyword", "studio":
		list, ok := ids()
		if !ok {
			return false
		}
		param := map[string]string{"keyword": "keywords", "studio": "companies"}[r.field]
		switch r.op {
		case "is", "in":
			q.Set("with_"+param, strings.Join(list, join(r.op)))
		default:
			q.Set("without_"+param, strings.Join(list, ","))
		}
		return true
	case "person":
		if kind != "movie" || (r.op != "is" && r.op != "in") {
			return false
		}
		list, ok := ids()
		if !ok {
			return false
		}
		param := "with_people"
		role := ""
		for _, rf := range r.refs {
			role = rf.Role
		}
		switch role {
		case "cast":
			param = "with_cast"
		case "director", "writer", "crew":
			param = "with_crew"
		}
		q.Set(param, strings.Join(list, join(r.op)))
		return true
	case "network":
		if kind != "tv" || (r.op != "is" && r.op != "in") {
			return false
		}
		list, ok := ids()
		if !ok {
			return false
		}
		q.Set("with_networks", strings.Join(list, "|"))
		return true
	case "runtime":
		switch r.op {
		case "gte":
			q.Set("with_runtime.gte", ftoa(r.nums[0]))
		case "lte":
			q.Set("with_runtime.lte", ftoa(r.nums[0]))
		case "between":
			q.Set("with_runtime.gte", ftoa(r.nums[0]))
			q.Set("with_runtime.lte", ftoa(r.nums[1]))
		default:
			return false
		}
		return true
	case "rating", "votes":
		param := map[string]string{"rating": "vote_average", "votes": "vote_count"}[r.field]
		switch r.op {
		case "gte":
			q.Set(param+".gte", ftoa(r.nums[0]))
		case "lte":
			q.Set(param+".lte", ftoa(r.nums[0]))
		case "between":
			q.Set(param+".gte", ftoa(r.nums[0]))
			q.Set(param+".lte", ftoa(r.nums[1]))
		default:
			return false
		}
		return true
	case "year", "decade":
		span := 0
		if r.field == "decade" {
			span = 9
		}
		y := func(v float64) int { return int(v) }
		switch r.op {
		case "gte":
			setRange(fmt.Sprintf("%04d-01-01", y(r.nums[0])), "")
		case "lte":
			setRange("", fmt.Sprintf("%04d-12-31", y(r.nums[0])+span))
		case "between":
			setRange(fmt.Sprintf("%04d-01-01", y(r.nums[0])), fmt.Sprintf("%04d-12-31", y(r.nums[1])+span))
		case "is":
			if len(r.nums) != 1 {
				return false
			}
			setRange(fmt.Sprintf("%04d-01-01", y(r.nums[0])), fmt.Sprintf("%04d-12-31", y(r.nums[0])+span))
		default:
			return false
		}
		return true
	case "released":
		now := s.now()
		day := func(t time.Time) string { return t.Format("2006-01-02") }
		switch r.op {
		case "within":
			setRange(day(now.Add(-r.within)), day(now))
		case "gte":
			setRange(day(r.from.at(now)), "")
		case "lte":
			setRange("", day(r.to.at(now)))
		case "between":
			setRange(day(r.from.at(now)), day(r.to.at(now)))
		default:
			return false
		}
		return true
	case "language":
		if r.op != "is" && r.op != "in" {
			return false
		}
		if r.op == "is" && len(r.tags) > 1 {
			return false
		}
		q.Set("with_original_language", strings.Join(r.tags, "|"))
		return true
	case "country":
		if r.op != "is" && r.op != "in" {
			return false
		}
		var codes []string
		for _, c := range r.tags {
			code := countryCode(c)
			if code == "" {
				return false
			}
			codes = append(codes, code)
		}
		if r.op == "is" && len(codes) > 1 {
			return false
		}
		q.Set("with_origin_country", strings.Join(codes, "|"))
		return true
	case "contentRating":
		if kind != "movie" {
			return false
		}
		// TMDB discover filters US film certifications, so a level is
		// pushed down as its US film label. Levels without one (TV-Y7) and
		// "Not rated" are evaluated locally only.
		var labels []string
		for _, t := range r.tags {
			l := contentrating.LevelOf(t)
			if l == contentrating.Unrated || l == contentrating.Children {
				return false
			}
			labels = append(labels, l.Key())
		}
		switch r.op {
		case "lte", "gte":
			q.Set("certification."+r.op, labels[0])
		case "is", "in":
			if r.op == "is" && len(labels) > 1 {
				return false
			}
			q.Set("certification", strings.Join(labels, "|"))
		default:
			return false
		}
		q.Set("certification_country", "US")
		return true
	case "watchProvider":
		if r.op != "is" && r.op != "in" {
			return false
		}
		list, ok := ids()
		if !ok {
			return false
		}
		q.Set("with_watch_providers", strings.Join(list, "|"))
		q.Set("watch_region", s.region(r.region))
		q.Set("with_watch_monetization_types", "flatrate|free|ads")
		return true
	}
	return false
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func countryCode(c string) string {
	up := strings.ToUpper(strings.TrimSpace(c))
	if len(up) == 2 {
		return up
	}
	for code, name := range countryNames {
		if strings.EqualFold(name, c) {
			return code
		}
	}
	return ""
}

// providerSet lists what a watch-provider rule's services stream, from
// TMDB discover (the most popular titles first).
func (s *Service) providerSet(ctx context.Context, sp *core.CollectionSpec, r *rule, warn func(string, ...any)) (map[core.RatingKey]bool, []core.Title) {
	set := map[core.RatingKey]bool{}
	if s.api.c == nil {
		warn("watch-provider rules need TMDB, which is not configured")
		return set, nil
	}
	var all []core.Title
	for _, kind := range kindsFor(sp.MediaType) {
		q := url.Values{}
		if !s.translate(ctx, &rule{field: "watchProvider", op: "in", refs: r.refs, region: r.region}, kind, q) {
			continue
		}
		q.Set("sort_by", "popularity.desc")
		items, _, err := s.api.discover(ctx, kind, q, 400, 20)
		if err != nil {
			warn("TMDB watch providers: %v", err)
			var partial *partialListError
			if !errors.As(err, &partial) {
				continue
			}
		}
		for _, t := range items {
			set[t.Key] = true
		}
		all = append(all, items...)
	}
	return set, all
}

// personalRules are the per-user parts of a built collection.
type personalRules struct {
	include, exclude []*rule
	// any: the spec matches "any", so a member passes when it met a shared
	// rule at build time (it is not in unshared) or meets any include rule.
	any      bool
	unshared map[core.RatingKey]bool
}

// filterForUser applies a user's content-rating limit and personal rules
// (watched, in progress) to built members.
func (s *Service) filterForUser(ctx context.Context, keys []core.RatingKey, pr personalRules, user *core.User, snap *snapshot) []core.RatingKey {
	max := ""
	if user != nil {
		max = user.MaxContentRating
	}
	include, exclude := pr.include, pr.exclude
	personal := len(include)+len(exclude) > 0
	var exposure *libraryfilter.Filter
	if s.catCfg != nil {
		if rules := s.catCfg().Exclusions; len(rules) > 0 {
			exposure, _ = libraryfilter.Compile(rules)
		}
	}
	if max == "" && !personal && exposure == nil {
		return keys
	}
	var statuses map[core.RatingKey]core.WatchStatus
	if st := s.stateDep(); personal && user != nil && st != nil {
		if m, err := st.GetMany(ctx, user.ID, keys); err == nil {
			statuses = m
		} else {
			s.log.Debug("collections: watch state", "err", err)
		}
	}
	if statuses == nil {
		statuses = map[core.RatingKey]core.WatchStatus{} // nothing watched
	}
	ec := &evalCtx{now: s.now(), status: statuses}
	out := make([]core.RatingKey, 0, len(keys))
	for _, k := range keys {
		var e *entry
		if snap != nil {
			e = snap.entries[k]
		}
		if exposure != nil && e != nil {
			t := e.card()
			if exposure.Hidden(&t) {
				continue
			}
		}
		// A member the index no longer knows has no known rating, and an
		// unknown rating exceeds every limit.
		if max != "" && (e == nil || contentrating.Exceeds(e.crating, max)) {
			continue
		}
		if personal {
			if e == nil {
				e = &entry{key: k}
			}
			ok := true
			if pr.any {
				ok = !pr.unshared[k]
				for _, r := range include {
					if ok {
						break
					}
					ok = r.match(e, ec)
				}
			} else {
				for _, r := range include {
					if !r.match(e, ec) {
						ok = false
						break
					}
				}
			}
			for _, r := range exclude {
				if ok && r.match(e, ec) {
					ok = false
				}
			}
			if !ok {
				continue
			}
		}
		out = append(out, k)
	}
	return out
}
