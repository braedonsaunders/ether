package personalize

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/recommend"
)

// generator proposes candidate rows of one kind.
type generator struct {
	kind string
	run  func(b *builder) []*candRow
}

// generators run in this order; order only matters for logging, since
// assembly ranks rows.
var generators = []generator{
	{KindFavorites, (*builder).genFavorites},
	{KindContinue, (*builder).genContinue},
	{KindUpNext, (*builder).genUpNext},
	{KindTop10Today, (*builder).genTop10Today},
	{KindTop10, (*builder).genTop10},
	{KindShowcase, (*builder).genShowcase},
	{KindBecause, (*builder).genBecause},
	{KindPerson, (*builder).genPerson},
	{KindTrendingTaste, (*builder).genTrendingTaste},
	{KindCrowd, (*builder).genCrowd},
	{KindCrowdTrending, (*builder).genCrowdTrending},
	{KindHiddenGems, (*builder).genHiddenGems},
	{KindNewThisWeek, (*builder).genNewThisWeek},
	{KindComfort, (*builder).genComfort},
	{KindDifferent, (*builder).genDifferent},
	{KindShortOnTime, (*builder).genShortOnTime},
	{KindFranchise, (*builder).genFranchise},
	{KindLLM, (*builder).genLLM},
	{KindTraktRecs, (*builder).genTrakt},
	{KindTrending, (*builder).genTrending},
	{KindPopularServer, (*builder).genPopularServer},
	{KindNewReleases, (*builder).genNewReleases},
	{KindTopRated, (*builder).genTopRated},
	{KindPopular, (*builder).genPopular},
	{KindAiringToday, (*builder).genAiringToday},
	// Movies → Recommended lenses (lenses.go).
	{KindTasteCluster, (*builder).genTasteClusters},
	{KindBridge, (*builder).genBridge},
	{KindEra, (*builder).genEra},
	{KindWorld, (*builder).genWorld},
	{KindOccasion, (*builder).genOccasions},
	{KindAcclaimed, (*builder).genAcclaimed},
	{KindTopGenre, (*builder).genTopGenre},
	{KindStudio, (*builder).genStudio},
	// TV Shows → Recommended lenses (lenses_tv.go).
	{KindNewSeasons, (*builder).genNewSeasons},
	{KindFinishShow, (*builder).genFinishShow},
	{KindStartNew, (*builder).genStartNew},
	{KindBinge, (*builder).genBinge},
	{KindSticksLanding, (*builder).genSticksLanding},
	{KindLimited, (*builder).genLimited},
	{KindAiringTaste, (*builder).genAiringTaste},
	{KindNetwork, (*builder).genNetwork},
	{KindStandalone, (*builder).genStandalone},
}

func newRow(kind, id, title, reason string, items []candItem) *candRow {
	info := infoFor(kind)
	return &candRow{kind: kind, id: id, title: title, reason: reason, context: info.context, source: info.source,
		items: items, minItems: 4, scale: 1}
}

// genFavorites is owner-placeable only and retains the source's newest-first order.
func (b *builder) genFavorites() []*candRow {
	if b.generic || b.e.opts.Favorites == nil {
		return nil
	}
	favorites, err := b.e.opts.Favorites(b.ctx, &b.user)
	if err != nil {
		return nil
	}
	var items []candItem
	for _, favorite := range favorites {
		if favorite.Key == 0 {
			continue
		}
		title, err := b.e.catalog.Get(b.ctx, favorite.Key)
		if err != nil || title == nil || title.Adult || (b.e.opts.Allowed != nil && !b.e.opts.Allowed(&b.user, title)) {
			continue
		}
		items = append(items, candItem{t: *title, base: rankScore(len(items), len(favorites)), qual: quality(title), reason: "A favorite"})
	}
	if len(items) == 0 {
		return nil
	}
	row := newRow(KindFavorites, "favorites", "Favorites", "Titles marked as favorites.", items)
	row.minItems = 1
	row.keepOrder = true
	return []*candRow{row}
}

// ---- Continue Watching and Up Next --------------------------------------------

// genContinue mirrors the Plex front end's Continue Watching so the engine
// can rank it; the front end renders its own copy where the engine puts it.
func (b *builder) genContinue() []*candRow {
	if b.generic || b.e.state == nil {
		return nil
	}
	items := b.e.continueItems(b.ctx, &b.user, 50)
	r := newRow(KindContinue, "continue", "Continue Watching", "Titles you're part-way through, most recent first.", items)
	r.keepOrder, r.minItems = true, 0
	return []*candRow{r}
}

// continueItems lists in-progress titles, most recent first.
func (e *Engine) continueItems(ctx context.Context, user *core.User, limit int) []candItem {
	if e.state == nil || user == nil {
		return nil
	}
	sts, err := e.state.InProgress(ctx, user.ID, limit)
	if err != nil {
		e.log.Debug("personalize: in progress failed", "err", err)
		return nil
	}
	return e.continueFrom(ctx, sts)
}

// continueFrom resolves in-progress statuses to Continue Watching items.
func (e *Engine) continueFrom(ctx context.Context, sts []core.WatchStatus) []candItem {
	var out []candItem
	for i, ws := range sts {
		t, err := e.catalog.Get(ctx, ws.Key)
		if err != nil || t == nil {
			continue
		}
		reason := "You stopped part-way through"
		if ws.Duration > 0 {
			reason = fmt.Sprintf("%d%% watched", int(math.Round(100*float64(ws.ViewOffset)/float64(ws.Duration))))
		}
		out = append(out, candItem{t: *t, base: rankScore(i, len(sts)), qual: quality(t), reason: reason})
	}
	return out
}

func (b *builder) genUpNext() []*candRow {
	if b.generic || b.e.state == nil {
		return nil
	}
	hist, err := b.e.state.History(b.ctx, b.user.ID, time.Time{}, 300)
	if err != nil {
		return nil
	}
	if b.e.opts.Favorites != nil {
		if favorites, e := b.e.opts.Favorites(b.ctx, &b.user); e == nil {
			prioritizeFavoriteHistory(hist, favorites)
		}
	}
	seenShow := map[int64]bool{}
	var items []candItem
	for _, h := range hist {
		if !h.Completed || h.Key.Kind() != core.KindEpisode || seenShow[h.Key.TMDB()] {
			continue
		}
		seenShow[h.Key.TMDB()] = true
		next, err := b.e.catalog.NextEpisode(b.ctx, h.Key)
		if err != nil || next == nil {
			continue
		}
		if !next.Released.IsZero() && next.Released.After(b.now) {
			continue
		}
		if ws, err := b.e.state.Get(b.ctx, b.user.ID, next.Key); err == nil && (ws.ViewCount > 0 || ws.ViewOffset > 0) {
			continue
		}
		if !recommend.ContentRatingAllowed(next.ContentRating, b.user.MaxContentRating) {
			continue
		}
		show := next.ShowTitle
		if show == "" {
			show = "this show"
		}
		items = append(items, candItem{t: *next, base: 1 - float64(len(items))*0.02, qual: 0.6,
			reason: fmt.Sprintf("Next episode of %s", show)})
		if len(items) >= 30 {
			break
		}
	}
	r := newRow(KindUpNext, "ondeck", "Up Next", "The next episode of shows you're following.", items)
	r.keepOrder, r.minItems = true, 1
	return []*candRow{r}
}

// ---- personal pool ---------------------------------------------------------------

// seeds are liked titles, most recent first.
func (b *builder) seeds(n int) []Evidence {
	var out []Evidence
	for _, e := range b.taste.Evidence {
		if e.Weight >= 0.4 && (e.Kind == EvidenceFinished || e.Kind == EvidenceRated || (e.Kind == EvidenceWatching && e.Progress >= 0.5)) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// pool is the shared candidate pool for taste-ranked rows: similar to
// recent favourites, trending, popular, top rated, and the favourite
// genres' best.
func (b *builder) pool() []core.Title {
	if v, ok := b.memo["pool"]; ok {
		return v
	}
	var all []core.Title
	for _, s := range b.seeds(8) {
		all = append(all, b.similar(s.Key, 20)...)
	}
	all = append(all, b.both(func(t core.MediaType) []core.Title { return b.list("trending_week", t, 40) })...)
	all = append(all, b.both(func(t core.MediaType) []core.Title { return b.list("top_rated", t, 40) })...)
	all = append(all, b.both(func(t core.MediaType) []core.Title {
		return b.browse(core.BrowseQuery{Filter: core.Filter{Type: t}, Sort: core.SortPopularity, Desc: true, Limit: 60})
	})...)
	for _, g := range b.taste.TopGenres(3) {
		all = append(all, b.browse(core.BrowseQuery{Filter: core.Filter{Genres: []string{g}}, Sort: core.SortPopularity, Desc: true, Limit: 60})...)
		all = append(all, b.browse(core.BrowseQuery{Filter: core.Filter{Genres: []string{g}, MinRating: 7}, Sort: core.SortRating, Desc: true, Limit: 40})...)
	}
	curDecade := b.now.Year() / 10 * 10
	for _, kv := range sortedByWeight(b.taste.Decade, 2) {
		if kv.w >= 0.4 && kv.k > 0 && kv.k < curDecade {
			all = append(all, b.browse(core.BrowseQuery{Filter: core.Filter{YearFrom: kv.k, YearTo: kv.k + 9, MinRating: 7}, Sort: core.SortPopularity, Desc: true, Limit: 25})...)
		}
	}
	b.memo["pool"] = all
	return all
}

// ---- taste rows ------------------------------------------------------------------

func (b *builder) genTop10() []*candRow {
	if !b.personal() || b.taste.Confidence < top10Confidence {
		return nil // "Top 10 on <server> Today" leads instead
	}
	recentSeeds := b.seeds(6)
	simSeed := map[core.RatingKey]string{}
	simBoost := map[core.RatingKey]float64{}
	for si, s := range recentSeeds {
		w := b.seedRecencyWeight(s.At, si)
		for idx, nb := range b.similar(s.Key, 25) {
			score := (0.70 + 0.30*rankScore(idx, 25)) * w
			if score > simBoost[nb.Key] {
				simBoost[nb.Key] = score
				simSeed[nb.Key] = s.Title.Title
			}
		}
	}
	// The crowd graph (crowd.go) adds what fans of the viewer's favourites
	// love, and its consensus is the strongest single vote for a title.
	g := b.crowd()
	pool := b.pool()
	for i, t := range g.titles() {
		if i >= 120 {
			break
		}
		pool = append(pool, t)
	}
	items := b.rank(pool, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
		if why == "" {
			why = "Highly rated and widely loved"
		}
		mom, momSeed := b.momentumScore(t, recentSeeds, simBoost[t.Key], simSeed[t.Key])
		ce := g.entry(t.Key)
		crowd, consensus := 0.0, 0
		if ce != nil {
			crowd, consensus = ce.score, ce.consensus
		}
		pct := int(math.Round(58 + 24*clamp(fit, 0, 1) + 10*clamp(mom, 0, 1) + 6*crowd + 6*consensusStrength(consensus)))
		if pct < 65 {
			pct = 65
		} else if pct > 99 {
			pct = 99
		}
		switch {
		case ce != nil && ce.consensus >= 2:
			why = fmt.Sprintf("%d%% Match · %s", pct, crowdReason(ce))
		case simSeed[t.Key] != "":
			why = fmt.Sprintf("%d%% Match · Because you watched %s · %s", pct, simSeed[t.Key], why)
		case momSeed != "" && mom >= 0.28:
			why = fmt.Sprintf("%d%% Match · Momentum from %s · %s", pct, momSeed, why)
		case crowd >= 0.3:
			why = fmt.Sprintf("%d%% Match · %s", pct, crowdReason(ce))
		default:
			why = fmt.Sprintf("%d%% Match · %s", pct, why)
		}
		return top10Score(fit, mom, crowd, consensus, t, b.now), why
	})
	kept := items[:0:0]
	for _, it := range items {
		ce := g.entry(it.t.Key)
		consensus := ce != nil && ce.consensus >= 2
		minWR := 6.8
		if simBoost[it.t.Key] > 0 || it.fit >= 0.22 || consensus {
			minWR = 6.0
		}
		// Taste must agree, unless several favourites' fans do.
		fitOK := it.fit > 0 || consensus && it.fit > -0.1
		if fitOK && weightedRating(&it.t) >= minWR && b.taste.headlineOK(&it.t) {
			kept = append(kept, it)
		}
	}
	r := newRow(KindTop10, "top10", "Top 10 for You", "The ten titles your taste model, what fans of your favourites love and your recent viewing momentum rate highest right now.", capItems(kept, 40))
	r.maxItems, r.minItems = 10, 5
	return []*candRow{r}
}

// top10Score blends a Top 10 candidate's signals: taste fit, momentum
// from the latest watches, crowd co-engagement and its consensus
// (crowd.go), quality, and a little popularity and recency. Trend buzz is
// added at assembly, scaled by the page's Freshness dial (assemble.go).
func top10Score(fit, mom, crowd float64, consensus int, t *core.Title, now time.Time) float64 {
	return 0.32*fit + 0.20*mom + 0.18*crowd + 0.14*consensusStrength(consensus) +
		0.11*quality(t) + 0.025*popularity(t) + 0.025*recency(t, now)
}

func (b *builder) seedRecencyWeight(at time.Time, rank int) float64 {
	rankDecay := math.Max(0.4, 1.0-0.22*float64(rank))
	if at.IsZero() {
		return 0.65 * rankDecay
	}
	days := math.Max(0, b.now.Sub(at).Hours()/24.0)
	return math.Exp(-days/14.0) * rankDecay
}

// momentumScore measures how strongly t continues the viewer's immediate
// recent viewing streak (last 3 liked/finished titles).
func (b *builder) momentumScore(t *core.Title, recent []Evidence, directSim float64, directSeed string) (float64, string) {
	best := directSim
	bestSeed := directSeed
	tGenres := normGenres(t.Genres)
	for si, s := range recent {
		w := b.seedRecencyWeight(s.At, si)
		overlap := 0.0
		// Shared director or lead cast with a recent watch.
		for _, d := range s.Title.Directors {
			if d.TMDB != 0 && hasPerson(t, d.TMDB) {
				overlap += 0.48
				break
			}
		}
		for i, c := range s.Title.Cast {
			if i >= 3 {
				break
			}
			if c.TMDB != 0 && hasPerson(t, c.TMDB) {
				overlap += 0.30
				break
			}
		}
		// Shared keywords with a recent watch.
		if len(s.Title.Keywords) > 0 && len(t.Keywords) > 0 {
			kwHits := 0
			for _, kw := range t.Keywords {
				for _, skw := range s.Title.Keywords {
					if strings.EqualFold(kw, skw) {
						kwHits++
						break
					}
				}
			}
			if kwHits > 0 {
				overlap += math.Min(0.45, 0.22*float64(kwHits))
			}
		}
		// Shared genres with a recent watch.
		sGenres := normGenres(s.Title.Genres)
		if len(sGenres) > 0 && len(tGenres) > 0 {
			gHits := 0
			for _, g := range tGenres {
				for _, sg := range sGenres {
					if g == sg {
						gHits++
						break
					}
				}
			}
			if gHits > 0 {
				overlap += 0.22 * float64(gHits) / float64(max(len(sGenres), 1))
			}
		}
		score := clamp(overlap*w, 0, 1)
		if score > best {
			best = score
			bestSeed = s.Title.Title
		}
	}
	return clamp(best, 0, 1), bestSeed
}

func (b *builder) genBecause() []*candRow {
	if !b.personal() {
		return nil
	}
	var rows []*candRow
	g := b.crowd()
	for i, s := range b.seeds(6) {
		seed := s.Title
		sim := b.similar(s.Key, 30)
		items := b.rank(sim, func(t *core.Title, idx, n int, fit float64, _ string) (float64, string) {
			// Titles fans of several favourites also love rise in the row.
			why := "Because you watched " + seed.Title
			crowd := 0.0
			if ce := g.entry(t.Key); ce != nil {
				crowd = ce.score
				if ce.consensus >= 2 {
					for _, other := range ce.seeds {
						if other != seed.Title {
							why += " · Also loved by fans of " + other
							break
						}
					}
				}
			}
			return 0.35*fit + 0.25*rankScore(idx, n) + 0.25*quality(t) + 0.15*crowd, why
		})
		if len(items) == 0 {
			continue
		}
		verb := "finished"
		if s.Kind == EvidenceRated {
			verb = fmt.Sprintf("rated %g/10", s.Rating)
		} else if s.Kind == EvidenceWatching {
			verb = "are watching"
		}
		r := withTyped(newRow(KindBecause, fmt.Sprintf("because.%d", int64(s.Key)), "Because You Watched "+seed.Title,
			fmt.Sprintf("You %s %s%s; these are the closest matches you haven't seen.", verb, seed.Title, agoPhrase(s.At, b.now)),
			capItems(items, 40)), items)
		r.scale = 1 - 0.07*float64(i)
		rows = append(rows, r)
	}
	return rows
}

func agoPhrase(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := now.Sub(at)
	switch {
	case d < 36*time.Hour:
		return " recently"
	case d < 14*24*time.Hour:
		return fmt.Sprintf(" %d days ago", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf(" %d weeks ago", int(d.Hours()/24/7))
	}
	return fmt.Sprintf(" %d months ago", int(d.Hours()/24/30))
}

func (b *builder) genPerson() []*candRow {
	if !b.personal() {
		return nil
	}
	var rows []*candRow
	for _, kv := range sortedByWeight(b.taste.Person, 0) {
		if len(rows) >= 6 {
			break
		}
		info := b.taste.People[kv.k]
		if kv.w < 0.35 || info.name == "" {
			continue
		}
		// How many titles the viewer finished with this person.
		credits := 0
		for _, e := range b.taste.Evidence {
			if e.Kind == EvidenceFinished && hasPerson(&e.Title, kv.k) {
				credits++
			}
		}
		works := b.browse(core.BrowseQuery{Filter: core.Filter{Person: kv.k}, Sort: core.SortPopularity, Desc: true, Limit: 40})
		name := info.name
		items := b.rank(works, func(t *core.Title, _, _ int, fit float64, _ string) (float64, string) {
			return 0.45*fit + 0.3*quality(t) + 0.25*popularity(t), personVerb(info.role, t, kv.k) + " " + name
		})
		if len(items) == 0 {
			continue
		}
		title := "More with " + name
		switch info.role {
		case "director":
			title = "More from " + name
		case "writer":
			title = "More Written by " + name
		case "creator":
			title = "Created by " + name
		case "cinematographer":
			title = "Shot by " + name
		case "composer":
			title = "Scored by " + name
		}
		reason := fmt.Sprintf("%s is one of your most-watched %ss", name, roleNoun(info.role))
		if credits > 1 {
			reason = fmt.Sprintf("You've finished %d titles with %s", credits, name)
		}
		r := withTyped(newRow(KindPerson, fmt.Sprintf("person.%d", kv.k), title, reason, capItems(items, 40)), items)
		r.scale = 0.8 + 0.2*kv.w
		rows = append(rows, r)
	}
	return rows
}

func hasPerson(t *core.Title, id int64) bool {
	for _, ps := range [][]core.Person{t.Directors, t.Writers, t.Cast, t.Crew} {
		for _, p := range ps {
			if p.TMDB == id {
				return true
			}
		}
	}
	return false
}

func personVerb(role string, t *core.Title, id int64) string {
	for _, d := range t.Directors {
		if d.TMDB == id {
			return "Directed by"
		}
	}
	for _, w := range t.Writers {
		if w.TMDB == id {
			return "Written by"
		}
	}
	for _, cr := range t.Crew {
		if cr.TMDB == id {
			switch strings.ToLower(cr.Role) {
			case "creator", "showrunner":
				return "Created by"
			case "cinematographer", "director of photography":
				return "Shot by"
			case "composer", "original music composer":
				return "Scored by"
			}
		}
	}
	return "With"
}

func roleNoun(role string) string {
	switch role {
	case "director", "writer", "creator", "cinematographer", "composer":
		return role
	}
	return "actor"
}

// genBridge builds multi-seed intersection rows ("Where <A> Meets <B>") that
// connect two distinct favourites from the viewer's history.
func (b *builder) genBridge() []*candRow {
	if !b.personal() {
		return nil
	}
	seeds := b.seeds(8)
	if len(seeds) < 2 {
		return nil
	}
	type pair struct {
		a, c Evidence
	}
	var pairs []pair
	used := map[core.RatingKey]bool{}
	for i := 0; i < len(seeds) && len(pairs) < 2; i++ {
		if used[seeds[i].Key] {
			continue
		}
		gi := normGenres(seeds[i].Title.Genres)
		for j := i + 1; j < len(seeds); j++ {
			if used[seeds[j].Key] {
				continue
			}
			if seeds[i].Title.CollectionID != 0 && seeds[i].Title.CollectionID == seeds[j].Title.CollectionID {
				continue
			}
			gj := normGenres(seeds[j].Title.Genres)
			// Require disjoint genres so the row genuinely bridges two
			// distinct worlds in the viewer's taste rather than duplicating
			// single-genre rows.
			if len(gi) == 0 || len(gj) == 0 || anyShared(gi, gj) {
				continue
			}
			used[seeds[i].Key], used[seeds[j].Key] = true, true
			pairs = append(pairs, pair{seeds[i], seeds[j]})
			break
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	g := b.crowd()
	var rows []*candRow
	for idx, p := range pairs {
		simA := map[core.RatingKey]float64{}
		simB := map[core.RatingKey]float64{}
		listA := b.similar(p.a.Key, 35)
		listB := b.similar(p.c.Key, 35)
		for i, t := range listA {
			simA[t.Key] = 0.35 + 0.65*rankScore(i, len(listA))
		}
		for i, t := range listB {
			simB[t.Key] = 0.35 + 0.65*rankScore(i, len(listB))
		}
		src := append(append([]core.Title{}, listA...), listB...)
		if g != nil {
			src = append(src, g.titles()...)
		}
		src = append(src, b.pool()...)
		items := b.rank(src, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
			sa := math.Max(simA[t.Key], titleSimilarity(&p.a.Title, t))
			sb := math.Max(simB[t.Key], titleSimilarity(&p.c.Title, t))
			if sa < 0.15 || sb < 0.15 {
				return math.NaN(), ""
			}
			bridge := math.Sqrt(sa * sb)
			r := fmt.Sprintf("Bridges %s and %s", p.a.Title.Title, p.c.Title.Title)
			if why != "" {
				r += " · " + why
			}
			return 0.45*bridge + 0.30*clamp(fit, -1, 1) + 0.25*quality(t), r
		})
		if len(items) < 5 {
			continue
		}
		id := fmt.Sprintf("bridge.%d.%d", int64(p.a.Key), int64(p.c.Key))
		title := fmt.Sprintf("Where %s Meets %s", p.a.Title.Title, p.c.Title.Title)
		reason := fmt.Sprintf("Titles that connect what you loved about %s and %s.", p.a.Title.Title, p.c.Title.Title)
		r := withTyped(newRow(KindBridge, id, title, reason, capItems(items, 40)), items)
		r.minItems = 5
		r.scale = 0.92 - 0.06*float64(idx)
		rows = append(rows, r)
	}
	return rows
}

func (b *builder) genTrendingTaste() []*candRow {
	if !b.personal() {
		return nil
	}
	src := append(b.both(func(t core.MediaType) []core.Title { return b.list("trending_week", t, 40) }),
		b.both(func(t core.MediaType) []core.Title { return b.list("trending_day", t, 20) })...)
	items := b.rank(src, func(t *core.Title, idx, n int, fit float64, why string) (float64, string) {
		if why == "" {
			why = "Trending this week"
		} else {
			why = "Trending · " + why
		}
		return 0.55*fit + 0.35*rankScore(idx, n) + 0.1*quality(t), why
	})
	kept := items[:0:0]
	for _, it := range items {
		if it.fit > 0.08 {
			kept = append(kept, it)
		}
	}
	r := withTyped(newRow(KindTrendingTaste, "trending.taste", "Trending in Your Taste",
		"This week's trending titles that match what you like.", capItems(kept, 40)), kept)
	r.minItems = 5
	return []*candRow{r}
}

func (b *builder) genHiddenGems() []*candRow {
	if !b.personal() {
		return nil
	}
	var src []core.Title
	for _, g := range b.taste.TopGenres(4) {
		// Popular-first within the genre: obscure titles sort first by raw
		// rating, and are filtered out by the vote floor anyway.
		src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Genres: []string{g}, MinRating: 7}, Sort: core.SortPopularity, Desc: true, Limit: 300})...)
	}
	src = append(src, b.pool()...)
	// Trustworthy rating (half the vote floor, weighted 7.0+) …
	var rated []core.Title
	for _, t := range src {
		if b.acclaimed(&t, 0.5, 7.0) {
			rated = append(rated, t)
		}
	}
	// … and little known relative to this catalogue: under the 40th
	// popularity percentile of those.
	var pops []float64
	for _, t := range rated {
		pops = append(pops, t.Popularity)
	}
	sort.Float64s(pops)
	popCap := math.Inf(1)
	if len(pops) >= 10 {
		popCap = pops[len(pops)*2/5]
	}
	var gems []core.Title
	for _, t := range rated {
		if t.Popularity <= popCap && float64(t.VoteCount) <= 5*priorVotes(t.Type) {
			gems = append(gems, t)
		}
	}
	items := b.rank(gems, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
		r := fmt.Sprintf("Rated %.1f from %s votes · A lesser-known match", t.Rating, votesPhrase(t.VoteCount))
		if why != "" {
			r += " · " + why
		}
		return 0.5*fit + 0.4*quality(t) + 0.1*(1-popularity(t)), r
	})
	kept := items[:0:0]
	for _, it := range items {
		if it.fit >= 0.12 {
			kept = append(kept, it)
		}
	}
	r := withTyped(newRow(KindHiddenGems, "hiddengems", "Hidden Gems for You",
		"Highly rated titles few people have found, that fit your taste.", capItems(kept, 40)), kept)
	r.minItems = 5
	return []*candRow{r}
}

// votesPhrase renders a vote count for a reason ("12k").
func votesPhrase(n int) string {
	switch {
	case n >= 10000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

func (b *builder) genNewThisWeek() []*candRow {
	if !b.personal() {
		return nil
	}
	genres := b.taste.TopGenres(3)
	if len(genres) == 0 {
		return nil
	}
	var src []core.Title
	for _, g := range genres {
		// Generous limits: catalogues showing unreleased titles list
		// upcoming ones first, and those are filtered out.
		src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Genres: []string{g}}, Sort: core.SortReleased, Desc: true, Limit: 300})...)
	}
	within := func(days int) []core.Title {
		var out []core.Title
		for _, t := range src {
			if !t.Released.IsZero() && !t.Released.After(b.now) && b.now.Sub(t.Released) <= time.Duration(days)*24*time.Hour {
				out = append(out, t)
			}
		}
		return out
	}
	fresh := within(7)
	window := "this week"
	if len(fresh) < 6 {
		fresh, window = within(21), "in the last three weeks"
	}
	items := b.rank(fresh, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
		r := "New " + window
		if why != "" {
			r += " · " + why
		}
		return 0.45*fit + 0.25*recency(t, b.now) + 0.15*popularity(t) + 0.15*quality(t), r
	})
	title := "New This Week in " + genres[0]
	if len(genres) > 1 {
		title += " & " + genres[1]
	}
	r := newRow(KindNewThisWeek, "newthisweek", title,
		fmt.Sprintf("Released %s in %s, the genres you watch most.", window, strings.Join(genres, ", ")), capItems(items, 40))
	return []*candRow{r}
}

func (b *builder) genComfort() []*candRow {
	if b.generic || b.taste == nil {
		return nil
	}
	var items []candItem
	for _, e := range b.taste.Evidence {
		if e.Kind != EvidenceFinished || e.Weight <= 0 {
			continue
		}
		long := e.Title.Type == core.TypeShow && e.Completions >= 6
		if !(e.Rewatched || e.Rating >= 8 || long) {
			continue
		}
		if b.now.Sub(e.At) < 45*24*time.Hour {
			continue // still fresh in memory
		}
		if !recommend.ContentRatingAllowed(e.Title.ContentRating, b.user.MaxContentRating) {
			continue
		}
		reason := "One of your favourites"
		switch {
		case e.Rewatched && e.Completions > 1 && e.Title.Type == core.TypeMovie:
			reason = fmt.Sprintf("You've watched this %d times", e.Completions)
		case e.Rating >= 8:
			reason = fmt.Sprintf("You rated it %g/10", e.Rating)
		case long:
			reason = fmt.Sprintf("You've watched %d episodes", e.Completions)
		}
		score := 0.5*clamp(e.Raw/2, 0, 1) + 0.2*e.Rating/10
		if e.Rewatched {
			score += 0.3
		}
		items = append(items, candItem{t: e.Title, base: score, fit: 1, qual: quality(&e.Title), reason: reason})
	}
	sortItems(items)
	r := newRow(KindComfort, "comfort", "Comfort Rewatches", "Favourites you've finished, rewatched or rated highly and haven't seen in a while.", items)
	r.minItems = 3
	return []*candRow{r}
}

func (b *builder) genDifferent() []*candRow {
	if !b.personal() || b.taste.Confidence < 0.2 {
		return nil
	}
	top := b.taste.TopGenres(3)
	topSet := map[string]bool{}
	for _, g := range top {
		topSet[g] = true
	}
	src := append(b.browse(core.BrowseQuery{Filter: core.Filter{MinRating: 7}, Sort: core.SortPopularity, Desc: true, Limit: 400}),
		b.both(func(t core.MediaType) []core.Title { return b.list("top_rated", t, 100) })...)
	var outside []core.Title
	for _, t := range src {
		ok := len(t.Genres) > 0
		for _, g := range normGenres(t.Genres) {
			if topSet[g] || b.taste.Genre[g] <= -0.3 {
				ok = false
				break
			}
		}
		if ok && b.acclaimed(&t, 1, 7.2) {
			outside = append(outside, t)
		}
	}
	items := b.rank(outside, func(t *core.Title, _, _ int, fit float64, _ string) (float64, string) {
		maxG := 0.0
		for _, g := range normGenres(t.Genres) {
			maxG = math.Max(maxG, b.taste.Genre[g])
		}
		r := "Acclaimed"
		if len(t.Genres) > 0 {
			r = "Acclaimed " + strings.ToLower(t.Genres[0])
		}
		r += fmt.Sprintf(" (%.1f from %s votes)", t.Rating, votesPhrase(t.VoteCount))
		if len(top) > 0 {
			r += ", a change from your usual " + top[0]
		}
		return 0.55*quality(t) + 0.3*popularity(t) + 0.15*(1-maxG), r
	})
	kept := items[:0:0]
	for _, it := range items {
		if it.fit >= -0.15 {
			kept = append(kept, it)
		}
	}
	r := withTyped(newRow(KindDifferent, "different", "Something Different",
		fmt.Sprintf("Exploration: acclaimed titles outside %s.", strings.Join(top, ", ")), capItems(kept, 40)), kept)
	r.minItems = 5
	return []*candRow{r}
}

func (b *builder) genShortOnTime() []*candRow {
	var src []core.Title
	if b.personal() {
		src = append(src, b.pool()...)
	}
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
		// Titles whose runtime is already known…
		max := shortMovieMinutes * time.Minute
		if typ == core.TypeShow {
			max = shortEpisodeMinutes * time.Minute
		}
		src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Type: typ, MaxRuntime: max}, Sort: core.SortPopularity, Desc: true, Limit: 100})...)
		// …and the most popular titles, whose details are fetched below.
		src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Type: typ}, Sort: core.SortPopularity, Desc: true, Limit: 80})...)
	}
	// Keep notable, unseen candidates, best-known first, then fetch the
	// details that decide "short" (runtime, episode count) for those that
	// lack them.
	var cands []core.Title
	seen := map[core.RatingKey]bool{}
	b.loadStatuses(src)
	for _, t := range src {
		if seen[t.Key] || !b.fresh(&t) || !b.notable(&t) {
			continue
		}
		seen[t.Key] = true
		cands = append(cands, t)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return quality(&cands[i])+popularity(&cands[i]) > quality(&cands[j])+popularity(&cands[j])
	})
	cands = b.hydrate(cands, needsShortDetails)
	var short []core.Title
	for _, t := range cands {
		if isShort(&t) && weightedRating(&t) >= 6.3 {
			short = append(short, t)
		}
	}
	fitW := 0.6
	if !b.personal() {
		fitW = 0
	}
	items := b.rank(short, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
		r := fmt.Sprintf("%d min", int(minutes(t)))
		if t.Type == core.TypeShow {
			r = fmt.Sprintf("%d-minute episodes, %d in all", int(minutes(t)), t.LeafCount)
		}
		if why != "" {
			r += " · " + why
		}
		return fitW*fit + 0.25*quality(t) + 0.15*popularity(t), r
	})
	r := newRow(KindShortOnTime, "shortontime", "Short on Time",
		"Films under 100 minutes and short series you can start and finish now, fitted to the time you usually have.", capItems(items, 60))
	r.minItems = 5
	return []*candRow{r}
}

func (b *builder) genFranchise() []*candRow {
	if b.generic || b.taste == nil {
		return nil
	}
	var rows []*candRow
	seen := map[int64]bool{}
	for _, e := range b.taste.Evidence {
		cid := e.Title.CollectionID
		if cid == 0 || e.Weight <= 0 || seen[cid] || len(rows) >= 4 {
			continue
		}
		seen[cid] = true
		name, members, err := b.e.catalog.Collection(b.ctx, cid)
		if err != nil || len(members) < 2 {
			continue
		}
		b.loadStatuses(members)
		sort.SliceStable(members, func(i, j int) bool {
			if !members[i].Released.Equal(members[j].Released) && !members[i].Released.IsZero() && !members[j].Released.IsZero() {
				return members[i].Released.Before(members[j].Released)
			}
			if members[i].Year != members[j].Year {
				return members[i].Year < members[j].Year
			}
			return members[i].Key < members[j].Key
		})
		watched, released := 0, 0
		var items []candItem
		for _, m := range members {
			if !m.Released.IsZero() && m.Released.After(b.now) {
				continue
			}
			released++
			ev := b.taste.evidenceFor(m.Key)
			if b.statuses[m.Key].ViewCount > 0 || (ev != nil && ev.Kind == EvidenceFinished) {
				watched++
				continue
			}
			if !recommend.ContentRatingAllowed(m.ContentRating, b.user.MaxContentRating) {
				continue
			}
			fit, _ := b.fit(&m)
			items = append(items, candItem{t: m, base: 1 - 0.03*float64(len(items)), fit: fit, qual: quality(&m)})
		}
		if watched == 0 || len(items) == 0 {
			continue
		}
		if name == "" {
			name = e.Title.CollectionName
		}
		for i := range items {
			items[i].reason = "Next in " + name
			if i > 0 {
				items[i].reason = "Still to watch in " + name
			}
		}
		title := "Finish " + name
		if !strings.HasSuffix(strings.ToLower(name), "collection") {
			title = "Finish the " + name + " Films"
		}
		r := newRow(KindFranchise, fmt.Sprintf("franchise.%d", cid), title,
			fmt.Sprintf("You've seen %d of %d %s films.", watched, released, name), items)
		r.keepOrder, r.minItems, r.collKey = true, 1, core.CollectionKey(cid)
		r.scale = 0.7 + 0.3*float64(watched)/float64(released)
		rows = append(rows, r)
	}
	return rows
}

// ---- external sources -------------------------------------------------------------

// llmSync is optionally implemented by the LLM source (internal/llm.Service)
// for background callers that can wait for a fresh answer.
type llmSync interface {
	ThemedRowsSync(ctx context.Context, user *core.User, seeds []core.Title, limit int) ([]core.Hub, error)
}

// llmContrastiveSync is optionally implemented by internal/llm.Service to
// incorporate negative evidence (abandoned/ignored titles and disliked genres).
type llmContrastiveSync interface {
	ThemedRowsContrastiveSync(ctx context.Context, user *core.User, seeds, dislikes []core.Title, vetoGenres []string, limit int) ([]core.Hub, error)
}

func (b *builder) genLLM() []*candRow {
	if b.e.llm == nil || !b.personal() {
		return nil
	}
	var seeds []core.Title
	for _, s := range b.seeds(8) {
		seeds = append(seeds, s.Title)
	}
	if len(seeds) == 0 {
		return nil
	}
	var dislikes []core.Title
	for _, e := range b.taste.Evidence {
		if e.Weight <= -0.15 && len(dislikes) < 5 {
			dislikes = append(dislikes, e.Title)
		}
	}
	var vetoGenres []string
	for g, w := range b.taste.Genre {
		if w <= -0.35 {
			vetoGenres = append(vetoGenres, g)
		}
	}
	sort.Strings(vetoGenres)
	ctx, cancel := context.WithTimeout(b.ctx, b.e.opts.LLMTimeout)
	defer cancel()
	var hubs []core.Hub
	var err error
	switch s := b.e.llm.(type) {
	case llmContrastiveSync:
		hubs, err = s.ThemedRowsContrastiveSync(ctx, &b.user, seeds, dislikes, vetoGenres, 30)
	case llmSync:
		hubs, err = s.ThemedRowsSync(ctx, &b.user, seeds, 30)
	default:
		hubs, err = b.e.llm.ThemedRows(ctx, &b.user, seeds, 30)
	}
	if err != nil {
		b.e.log.Debug("personalize: llm rows failed", "err", err)
		return nil
	}
	var rows []*candRow
	for _, h := range hubs {
		items := b.rankWith(h.Items, func(t *core.Title, idx, n int, fit float64, _ string) (float64, string) {
			return 0.7*rankScore(idx, n) + 0.3*fit, "Picked by your AI model for this theme"
		}, false)
		id := h.ID
		if !strings.HasPrefix(id, "llm.") {
			id = "llm." + id
		}
		rows = append(rows, newRow(KindLLM, id, h.Title, "A themed row your LLM suggested from your recent favourites.", items))
	}
	return rows
}

func (b *builder) genTrakt() []*candRow {
	if b.e.trakt == nil || b.generic {
		return nil
	}
	ctx, cancel := context.WithTimeout(b.ctx, b.e.opts.SourceTimeout)
	defer cancel()
	var rows []*candRow
	if recs, err := b.e.trakt.Recommendations(ctx, b.user.ID, 40); err == nil && len(recs) > 0 {
		items := b.rankWith(recs, func(t *core.Title, idx, n int, fit float64, _ string) (float64, string) {
			return 0.6*rankScore(idx, n) + 0.4*fit, "Recommended by Trakt"
		}, false)
		rows = append(rows, newRow(KindTraktRecs, "trakt.recommendations", "Trakt Recommendations", "From your connected Trakt account.", items))
	}
	if wl, err := b.e.trakt.Watchlist(ctx, b.user.ID, 60); err == nil && len(wl) > 0 {
		b.loadStatuses(wl)
		var items []candItem
		for _, t := range wl {
			ws := b.statuses[t.Key]
			if ws.ViewCount > 0 || !recommend.ContentRatingAllowed(t.ContentRating, b.user.MaxContentRating) {
				continue
			}
			items = append(items, candItem{t: t, base: rankScore(len(items), len(wl)), qual: quality(&t), reason: "On your Trakt watchlist"})
		}
		r := newRow(KindTraktWatch, "trakt.watchlist", "Your Trakt Watchlist", "Your own list on Trakt, in your order.", items)
		r.keepOrder, r.minItems = true, 1
		rows = append(rows, r)
	}
	return rows
}

// ---- generic rows (every viewer, and new viewers' whole Home) ------------------------

func (b *builder) genTrending() []*candRow {
	src := b.both(func(t core.MediaType) []core.Title {
		return append(b.list("trending_day", t, 200), b.list("trending_week", t, 200)...)
	})
	items := b.rankWith(src, func(t *core.Title, idx, n int, fit float64, _ string) (float64, string) {
		return 0.85*rankScore(idx, n) + 0.15*fit, "Trending globally on TMDB"
	}, true)
	r := newRow(KindTrending, "trending", "Trending Now", "Global movie and TV trends, refreshed hourly.", capItems(items, 200))
	r.typed = splitTyped(items, 200)
	r.minItems = 5 // production assembly enforces the full 12-title floor
	return []*candRow{r}
}

func (b *builder) genPopularServer() []*candRow {
	plays, err := b.e.store.plays(b.ctx, 0, b.now.AddDate(0, 0, -30))
	if err != nil || len(plays) == 0 {
		return nil
	}
	viewers := map[core.RatingKey]map[int64]bool{}
	count := map[core.RatingKey]int{}
	for _, p := range plays {
		if p.userID == b.user.ID && !b.generic {
			continue
		}
		if p.abandoned {
			continue
		}
		if viewers[p.tasteKey] == nil {
			viewers[p.tasteKey] = map[int64]bool{}
		}
		viewers[p.tasteKey][p.userID] = true
		count[p.tasteKey]++
	}
	keys := make([]core.RatingKey, 0, len(viewers))
	for k := range viewers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		vi, vj := len(viewers[keys[i]]), len(viewers[keys[j]])
		if vi != vj {
			return vi > vj
		}
		if count[keys[i]] != count[keys[j]] {
			return count[keys[i]] > count[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var titles []core.Title
	for _, k := range keys {
		if len(titles) >= 40 {
			break
		}
		if t, err := b.e.catalog.Get(b.ctx, k); err == nil && t != nil {
			titles = append(titles, *t)
		}
	}
	items := b.rankWith(titles, func(t *core.Title, idx, n int, fit float64, _ string) (float64, string) {
		v := len(viewers[t.Key])
		r := "Watched by another viewer here"
		if v > 1 {
			r = fmt.Sprintf("Watched by %d viewers here", v)
		}
		return 0.7*rankScore(idx, n) + 0.3*fit, r
	}, false)
	name := b.e.serverName()
	r := newRow(KindPopularServer, "popular.server", "Popular on "+name,
		"What other viewers on this server watched most in the last 30 days.", items)
	return []*candRow{r}
}

func (b *builder) genNewReleases() []*candRow {
	src := b.both(func(t core.MediaType) []core.Title {
		return b.browse(core.BrowseQuery{Filter: core.Filter{Type: t}, Sort: core.SortReleased, Desc: true, Limit: 400})
	})
	var recent []core.Title
	for _, t := range src {
		if !t.Released.IsZero() && !t.Released.After(b.now) && b.now.Sub(t.Released) <= recentDays*24*time.Hour && b.taste.watchesLanguage(t.Language) {
			recent = append(recent, t)
		}
	}
	// rank keeps only notable releases (as popular as the median new
	// release, or already well rated by enough people).
	items := b.rank(recent, func(t *core.Title, _, _ int, fit float64, _ string) (float64, string) {
		return 0.4*recency(t, b.now) + 0.35*popularity(t) + 0.1*quality(t) + 0.15*fit, "New " + releasedPhrase(t.Released, b.now)
	})
	r := withTyped(newRow(KindNewReleases, "newreleases", "New & Noteworthy", "Notable releases of the last few months, newest and most talked-about first.", capItems(items, 40)), items)
	r.minItems = 5
	return []*candRow{r}
}

func releasedPhrase(rel, now time.Time) string {
	d := int(now.Sub(rel).Hours() / 24)
	switch {
	case d <= 7:
		return "this week"
	case d <= 31:
		return "this month"
	}
	return "in " + rel.Format("January")
}

func (b *builder) listRow(kind, id, title, reason, list, itemReason string, types ...core.MediaType) *candRow {
	var src []core.Title
	if len(types) == 2 {
		src = b.both(func(t core.MediaType) []core.Title { return b.list(list, t, 40) })
	} else {
		src = b.list(list, types[0], 40)
	}
	items := b.rank(src, func(t *core.Title, idx, n int, fit float64, _ string) (float64, string) {
		return 0.7*rankScore(idx, n) + 0.3*fit, itemReason
	})
	r := withTyped(newRow(kind, id, title, reason, capItems(items, 40)), items)
	r.minItems = 5
	return r
}

// genTopRated ranks by weighted rating among titles with enough votes,
// from TMDB's list and the catalogue's most popular titles (TMDB's list
// alone admits new releases with a few hundred enthusiastic votes).
func (b *builder) genTopRated() []*candRow {
	var src []core.Title
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
		src = append(src, b.list("top_rated", typ, 100)...)
		src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Type: typ, MinRating: 7}, Sort: core.SortPopularity, Desc: true, Limit: 400})...)
	}
	var good []core.Title
	for _, t := range src {
		if b.acclaimed(&t, 1.5, 7.4) {
			good = append(good, t)
		}
	}
	items := b.rank(good, func(t *core.Title, _, _ int, fit float64, _ string) (float64, string) {
		return 0.8*quality(t) + 0.2*fit, fmt.Sprintf("Rated %.1f by %s viewers", t.Rating, votesPhrase(t.VoteCount))
	})
	r := withTyped(newRow(KindTopRated, "toprated", "Top Rated", "The best-rated titles with enough votes to trust the rating.", capItems(items, 40)), items)
	r.coldHome, r.minItems = true, 5
	return []*candRow{r}
}

func (b *builder) genPopular() []*candRow {
	return []*candRow{b.listRow(KindPopular, "popular", "Popular", "TMDB's most popular titles.", "popular", "Popular right now", core.TypeMovie, core.TypeShow)}
}

func (b *builder) genAiringToday() []*candRow {
	return []*candRow{b.listRow(KindAiringToday, "airing", "Airing Today", "Shows with a new episode today.", "airing_today", "New episode today", core.TypeShow)}
}

// ---- cold start: Top 10 today and genre showcases ---------------------------------

// genTop10Today is the server's Top 10 today: what is trending right now
// blended with catalogue popularity and what viewers here are watching,
// limited to titles with a trustworthy rating. It leads a new viewer's
// Home and is a reliable row for everyone.
func (b *builder) genTop10Today() []*candRow {
	trendRank := map[core.RatingKey]float64{}
	var src []core.Title
	for _, list := range []struct {
		name string
		w    float64
	}{{"trending_day", 1}, {"trending_week", 0.7}} {
		for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
			items := b.list(list.name, typ, 40)
			for i, t := range items {
				if v := list.w * rankScore(i, len(items)); v > trendRank[t.Key] {
					trendRank[t.Key] = v
				}
			}
			src = append(src, items...)
		}
	}
	src = append(src, b.both(func(t core.MediaType) []core.Title {
		return b.browse(core.BrowseQuery{Filter: core.Filter{Type: t}, Sort: core.SortPopularity, Desc: true, Limit: 60})
	})...)
	here := b.serverPlays(7)
	maxHere := 0
	for _, n := range here {
		maxHere = max(maxHere, n)
	}
	var ok []core.Title
	for _, t := range src {
		// Vote-floored: a tenth of the floor and a decent weighted rating,
		// so a week-old blockbuster qualifies and an obscure title with a
		// burst of traffic does not.
		if b.acclaimed(&t, 0.1, 6.0) && b.taste.watchesLanguage(t.Language) {
			ok = append(ok, t)
		}
	}
	bz := b.buzz() // trending lists plus Trakt's public lists when set up
	items := b.rank(ok, func(t *core.Title, _, _ int, fit float64, _ string) (float64, string) {
		h := 0.0
		if maxHere > 0 {
			h = float64(here[tasteKey(t.Key)]) / float64(maxHere)
		}
		trend := math.Max(trendRank[t.Key], bz.score(t.Key))
		reason := "Trending today"
		switch {
		case h > 0:
			reason = "Popular with viewers here"
		case trend == 0:
			reason = "One of the most popular titles right now"
		case bz.trakt:
			reason = bz.reason(t.Key)
		}
		return 0.45*trend + 0.25*popularity(t) + 0.15*quality(t) + 0.1*h + 0.05*fit, reason
	})
	r := newRow(KindTop10Today, "top10.today", "Top 10 on "+b.e.serverName()+" Today",
		"Today's most watched and talked-about titles with a trustworthy rating: trending now, catalogue popularity and what viewers here are watching.", capItems(items, 40))
	r.maxItems, r.minItems = 10, 5
	return []*candRow{r}
}

// serverPlays counts other viewers' plays per title over the last days.
func (b *builder) serverPlays(days int) map[core.RatingKey]int {
	out := map[core.RatingKey]int{}
	plays, err := b.e.store.plays(b.ctx, 0, b.now.AddDate(0, 0, -days))
	if err != nil {
		return out
	}
	for _, p := range plays {
		if (b.generic || p.userID != b.user.ID) && !p.abandoned {
			out[p.tasteKey]++
		}
	}
	return out
}

// genShowcase builds one acclaimed-and-popular row per broad genre. New
// viewers get three of them, one per genre cluster, rotating daily: what
// they play from them is the first evidence of their taste.
func (b *builder) genShowcase() []*candRow {
	if b.taste != nil && b.taste.Confidence >= coldStartConfidence {
		return nil
	}
	var rows []*candRow
	for ci, cluster := range showcaseClusters {
		for _, g := range cluster {
			src := b.browse(core.BrowseQuery{Filter: core.Filter{Genres: []string{g}}, Sort: core.SortPopularity, Desc: true, Limit: 200})
			var ok []core.Title
			for _, t := range src {
				if b.acclaimed(&t, 1, 7.0) {
					ok = append(ok, t)
				}
			}
			items := b.rank(ok, func(t *core.Title, _, _ int, fit float64, _ string) (float64, string) {
				return 0.5*quality(t) + 0.35*popularity(t) + 0.15*fit, fmt.Sprintf("Rated %.1f by %s viewers", t.Rating, votesPhrase(t.VoteCount))
			})
			if len(items) < 6 {
				continue
			}
			name := genrePlural[g]
			if name == "" {
				name = g
			}
			r := withTyped(newRow(KindShowcase, "showcase."+genreSlug(g), "Acclaimed "+name,
				fmt.Sprintf("A taste of the best %s, to learn what you like.", strings.ToLower(name)), capItems(items, 40)), items)
			r.coldOnly, r.group, r.minItems = true, fmt.Sprint(ci), 6
			rows = append(rows, r)
		}
	}
	return rows
}

func genreSlug(g string) string {
	return strings.ReplaceAll(strings.ToLower(strings.ReplaceAll(g, "&", "and")), " ", "-")
}

func prioritizeFavoriteHistory(hist []core.HistoryEntry, favorites []FavoriteSignal) {
	shows := map[core.RatingKey]bool{}
	for _, f := range favorites {
		if f.Key.Kind() == core.KindShow {
			shows[f.Key] = true
		}
	}
	if len(shows) == 0 {
		return
	}
	sort.SliceStable(hist, func(i, j int) bool {
		isFavorite := func(h core.HistoryEntry) bool {
			return h.Key.Kind() == core.KindEpisode && shows[core.ShowKey(h.Key.TMDB())]
		}
		return isFavorite(hist[i]) && !isFavorite(hist[j])
	})
}
