package personalize

import (
	"fmt"
	"math"
	"sort"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/recommend"
)

// assembly turns a precomputed userModel into the rows one request
// receives. It is pure: same inputs, same rows. Everything time-dependent
// (rotation, exploration) is seeded by the local date.
type assembly struct {
	visit    int64 // fixed for this browsing visit; new visits vary discovery rows
	hidden   map[core.RatingKey]bool
	previous []presentedRow // currently browsed positions; never bypass eligibility
	model    *userModel
	owner    []*candRow // owner collection rows for this context
	cw       []candItem // live Continue Watching items (Home only)
	user     core.User  // the requesting viewer
	hc       core.HubContext
	section  core.MediaType // "" = Home
	limit    int            // items per row
	layout   *core.HomeLayout
	global   map[string]arm // server-wide evidence by prior kind
	arms     map[string]arm // this viewer's evidence by kind
	allowed  func(core.MediaType) bool
	maxRows  int
	explore  float64
	day      int
	intent   *intentSnapshot // live session intent (#32); nil = none

	// Surfaces, dials and cross-surface dedupe (surfaces.go, tuning.go,
	// homelead.go).
	tune *tuning // the page's resolved dials; nil = defaults
	// pageLen is the page length the final layout pass will truncate to
	// (0 = unknown: every selected row drafts titles as equals).
	pageLen int
	// lead is what the viewer's Home leads with (Movies/TV pages with
	// DedupeHome); nil = no dedupe.
	lead    *homeLead
	weights map[*candRow]float64 // surface prior weight of admitted rows
	demoted map[*candRow]bool    // rows kept despite repeating the Home
	// held collects, per row, the Home lead titles kept off it that it
	// would have shown (homeDeduped).
	held        map[*candRow][]core.RatingKey
	homeDeduped int
	rk          *rankPass // learned ranker (ranker.go); nil = hand-tuned only
	minRow      int       // fewest titles an engine row may show (Options.MinRowItems)
}

// rankedRow is a candidate row during assembly.
type rankedRow struct {
	c        *candRow
	items    []candItem
	hubID    string
	score    float64
	d        decision
	strength float64
}

// mmrLambda trades relevance against redundancy inside a row.
const mmrLambda = 0.75

// maxOwnerRows caps owner collection rows on one screen.
const maxOwnerRows = 4

func (a *assembly) run() []core.HomeRow {
	var rows []*rankedRow
	cands := append([]*candRow{}, a.model.rows...)
	cands = append(cands, a.owner...)
	// New viewers get the curated cold-start rows (Top 10 today, genre
	// showcases) and acclaimed section rows on Home: there is little
	// personal to put there yet.
	newViewer := a.newViewer()
	a.weights = map[*candRow]float64{}
	a.demoted = map[*candRow]bool{}
	a.held = map[*candRow][]core.RatingKey{}
	for _, c := range cands {
		if c.coldOnly && !newViewer {
			continue
		}
		if a.section != "" && a.lead != nil && a.lead.rows[c.id] && infoFor(c.kind).dynamic && !a.ownerPlaced(c, a.hubID(c)) {
			continue // the Home already shows this very row (same seed)
		}
		w, ok := a.admit(c, a.hubID(c), newViewer)
		if !ok {
			continue
		}
		a.weights[c] = w
		if c.kind == KindContinue {
			if a.section != "" {
				continue
			}
			live := *c
			live.items = a.cw
			c = &live
			a.weights[c] = w
		}
		items := a.contextItems(c)
		if len(items) == 0 && c.kind != KindContinue {
			continue
		}
		rr := &rankedRow{c: c, items: items, hubID: a.hubID(c)}
		if a.layout != nil {
			p, _ := effectiveHomePolicy(*a.layout, a.user.ID, sectionName(a.section), core.HomeRow{HubID: rr.hubID, Kind: c.kind})
			if p != nil {
				rr.d.hidden = p.Status == "off" || !whenMatches(p.When, a.hc) || !weekEligible(a.user.ID, p.Row, a.hc, p.MaxDaysPerWeek)
				rr.d.pinned = !rr.d.hidden && p.Status == "always"
				if p.Slot > 0 {
					rr.d.position = p.Slot - 1
				}
			}
		}
		rr.score, rr.strength = a.rowScore(c, items)
		// Live session intent also reorders rows, not just items.
		rr.score *= a.intent.liftFor(items)
		rows = append(rows, rr)
	}

	var visible, pinned, hidden []*rankedRow
	for _, r := range rows {
		switch {
		case r.d.hidden:
			hidden = append(hidden, r)
		case r.d.pinned:
			pinned = append(pinned, r)
		default:
			visible = append(visible, r)
		}
	}
	sort.SliceStable(visible, func(i, j int) bool {
		if visible[i].score != visible[j].score {
			return visible[i].score > visible[j].score
		}
		return visible[i].hubID < visible[j].hubID
	})
	a.spreadRows(visible) // Variety dial: page-level genre spread
	a.retainRowOrder(visible)
	// Keep discovery defaults in the candidate window before row limits and
	// deduplication. Explicit layout policies still control final placement.
	var headlines []core.HomeRow
	for _, r := range visible {
		if !a.ownerPlaced(r.c, r.hubID) {
			headlines = append(headlines, core.HomeRow{HubID: r.hubID, Kind: r.c.kind, Hub: core.Hub{ItemCount: len(r.items)}})
		}
	}
	_, defaults := leadDefaults(headlines, nil, nil, a.maxRows)
	priority := func(r *rankedRow) int {
		if p := defaults[r.hubID]; p != nil && p.Slot > 0 {
			return p.Slot
		}
		return a.maxRows + 1
	}
	sort.SliceStable(visible, func(i, j int) bool { return priority(visible[i]) < priority(visible[j]) })
	// Per-kind caps (pinned rows count but are never dropped).
	perKind := map[string]int{}
	groups := map[string]bool{}
	owners := 0
	for _, r := range pinned {
		perKind[r.c.kind]++
		if r.c.group != "" {
			groups[r.c.kind+"/"+r.c.group] = true
		}
		if r.c.source == core.RowSourceOwner {
			owners++
		}
	}
	capped := visible[:0:0]
	for _, r := range visible {
		if perKind[r.c.kind] >= a.kindCap(r.c.kind) {
			continue
		}
		if r.c.group != "" {
			if groups[r.c.kind+"/"+r.c.group] {
				continue // one showcase per genre family
			}
			groups[r.c.kind+"/"+r.c.group] = true
		}
		if r.c.source == core.RowSourceOwner && owners >= maxOwnerRows {
			continue
		}
		perKind[r.c.kind]++
		if r.c.source == core.RowSourceOwner {
			owners++
		}
		capped = append(capped, r)
	}
	ordered := placePinned(capped, pinned, func(r *rankedRow) int { return r.d.position })

	out := a.fill(ordered, a.maxRows)
	a.countHeld(out)
	for _, r := range hidden {
		items := r.items
		if len(items) > a.limit {
			items = items[:a.limit]
		}
		hr := a.homeRow(r, items, len(r.items) > len(items))
		hr.Hidden = true
		out = append(out, hr)
	}
	return out
}

// anchored rows are defined by their exact titles (a franchise's
// remaining films, the next episodes, an owner's collection, the viewer's
// own watchlist): they keep their titles and similarity-driven rows give
// way.
// minRowItems keeps discovery shelves full on wide desktop and TV layouts.
// Sparse candidates are omitted and their titles redrafted into fuller rows.
const minRowItems = 12

// rowFloor is the minimum for a row kind. Continue Watching and Up Next are
// utility rows: one in-progress title is still worth showing.
func (a *assembly) rowFloor(kind string) int {
	if kind == KindContinue || kind == KindUpNext {
		return 0
	}
	return a.minRow
}

func anchored(c *candRow) bool { return c.keepOrder && c.kind != KindContinue }

// draftsFirst reports rows that take their titles before the round-robin:
// anchored rows, and the top picks that open the page (toppicks.go), so the
// page's best titles lead a fresh draft. An active presentation first keeps
// still-eligible cards in the shelves where the viewer already saw them.
func draftsFirst(c *candRow) bool {
	return anchored(c) || c.kind == KindTop10 || c.kind == KindTop10Today
}

// fill picks each row's items with cross-row dedupe, so a title appears
// once per screen:
//
//  1. The front end's own rows keep their items.
//  2. Anchored rows keep their titles (the first anchored row wins a
//     title they share).
//  3. The other rows draft the remaining titles in turns, in display
//     order: each round, every row takes its best title nobody has taken.
//     Higher rows pick first, but no row is starved by a greedy one above
//     it (Top 10 cannot swallow every Because-you-watched title).
//
// Only rows that will survive the page length (survivors: the same layout
// pass the front end runs, arrangeLayout) claim titles in steps 2–3; rows
// the page will cut draft afterwards from what is left, so a row nobody
// sees never takes a title from one they do.
//
// Surviving rows left with fewer than their minimum are dropped and the
// draft is rerun without them, so their titles go back to the others and
// the next row moves up. maxRows counts engine and owner rows.
func (a *assembly) fill(ordered []*rankedRow, maxRows int) []core.HomeRow {
	excluded := map[*rankedRow]bool{}
	attempts := len(ordered)
	for attempt := 0; ; attempt++ {
		var sel []*rankedRow
		counted := 0
		for _, r := range ordered {
			if excluded[r] {
				continue
			}
			if r.c.source != core.RowSourcePlex {
				if maxRows > 0 && counted >= maxRows {
					continue
				}
				counted++
			}
			sel = append(sel, r)
		}
		head := a.survivors(sel)
		picked := make(map[*rankedRow][]candItem, len(sel))
		claimed := map[core.RatingKey]bool{}
		for _, r := range sel {
			if r.c.source == core.RowSourcePlex {
				items := r.items
				if n := a.rowCap(r.c); len(items) > n {
					items = items[:n]
				}
				picked[r] = items
				for _, it := range items {
					claimed[it.t.Key] = true
				}
			}
		}
		draftOrder := append([]*rankedRow(nil), sel...)
		sort.SliceStable(draftOrder, func(i, j int) bool { return anchored(draftOrder[i].c) && !anchored(draftOrder[j].c) })
		for _, phase := range []bool{true, false} {
			inPhase := func(r *rankedRow) bool { return head == nil || head[r] == phase }
			// Ordered utility/collection rows retain their live order and priority.
			if len(a.previous) > 0 {
				for _, r := range sel {
					if r.c.source == core.RowSourcePlex || (r.c.source == core.RowSourceEngine && !r.c.keepOrder) || !draftsFirst(r.c) || !inPhase(r) {
						continue
					}
					for _, it := range r.items {
						if len(picked[r]) >= a.rowCap(r.c) {
							break
						}
						if !claimed[it.t.Key] {
							claimed[it.t.Key] = true
							picked[r] = append(picked[r], it)
						}
					}
				}
				a.retainItems(sel, picked, claimed, inPhase)
			}
			for _, r := range draftOrder {
				if r.c.source == core.RowSourcePlex || !draftsFirst(r.c) || !inPhase(r) {
					continue
				}
				for _, it := range r.items {
					if len(picked[r]) >= a.rowCap(r.c) {
						break
					}
					if !claimed[it.t.Key] {
						claimed[it.t.Key] = true
						picked[r] = append(picked[r], it)
					}
				}
			}
			cursor := make(map[*rankedRow]int, len(sel))
			for progress := true; progress; {
				progress = false
				for _, r := range sel {
					if r.c.source == core.RowSourcePlex || draftsFirst(r.c) || !inPhase(r) || len(picked[r]) >= a.rowCap(r.c) {
						continue
					}
					for cursor[r] < len(r.items) {
						it := r.items[cursor[r]]
						cursor[r]++
						if claimed[it.t.Key] {
							continue
						}
						claimed[it.t.Key] = true
						picked[r] = append(picked[r], it)
						progress = true
						break
					}
				}
			}
			if head == nil {
				break
			}
		}
		short := map[*rankedRow]bool{}
		var drop *rankedRow
		for _, r := range sel {
			if r.c.source == core.RowSourcePlex {
				continue
			}
			min := r.c.minItems
			if r.c.source == core.RowSourceEngine || !a.ownerPlaced(r.c, r.hubID) {
				floor := a.rowFloor(r.c.kind)
				if cap := a.rowCap(r.c); floor > cap {
					floor = cap
				}
				min = max(min, floor)
			}
			if r.d.pinned && min > 1 && (r.c.source != core.RowSourceEngine || a.minRow <= 0) {
				min = 1
			}
			// A client's card window can be smaller than the candidate's
			// editorial minimum. A full window is still a useful shelf.
			if cap := a.rowCap(r.c); min > cap {
				min = cap
			}
			if n := len(picked[r]); n == 0 || n < min {
				short[r] = true
				if head == nil || head[r] {
					drop = r // the lowest-placed surviving row too thin to show
				}
			}
		}
		// Drop one thin row at a time, lowest first, and redraft: its titles
		// go back to the rows above, which may then be full enough to stay.
		if drop != nil && attempt < attempts {
			excluded[drop] = true
			continue
		}
		out := make([]core.HomeRow, 0, len(sel))
		for _, r := range sel {
			items := picked[r]
			if r.c.source != core.RowSourcePlex && (len(items) == 0 || short[r]) {
				continue
			}
			out = append(out, a.homeRow(r, items, len(r.items) > len(items)))
		}
		return out
	}
}

// survivors predicts which selected rows the page keeps: the layout pass
// the front end runs (arrangeLayout), over these rows plus the front
// end's own rows above a section's (Continue Watching, Recently Added).
// nil means unknown (every row is a survivor).
func (a *assembly) survivors(sel []*rankedRow) map[*rankedRow]bool {
	if a.pageLen <= 0 {
		return nil
	}
	rows := make([]core.HomeRow, 0, len(sel)+2)
	if a.section != "" {
		prefix := sectionPrefix(a.section)
		sid := fmt.Sprint(a.hc.SectionID)
		rows = append(rows,
			core.HomeRow{HubID: prefix + ".inprogress." + sid, Kind: KindContinue, Source: core.RowSourcePlex},
			core.HomeRow{HubID: prefix + ".recentlyadded." + sid, Kind: KindRecentlyAdded, Source: core.RowSourcePlex})
	}
	byID := make(map[string]*rankedRow, len(sel))
	for _, r := range sel {
		rows = append(rows, core.HomeRow{HubID: r.hubID, Kind: r.c.kind, Source: r.c.source, Hub: core.Hub{ItemCount: len(r.items)}})
		byID[r.hubID] = r
	}
	var d core.HomeLayout
	if a.layout != nil {
		d = *a.layout
	}
	out := make(map[*rankedRow]bool, len(sel))
	for _, hr := range arrangeLayout(rows, d, a.user.ID, a.hc, a.surface(), a.pageLen) {
		if r := byID[hr.HubID]; r != nil && !hr.Hidden {
			out[r] = true
		}
	}
	return out
}

// rowCap is the most items a row shows on this screen.
func (a *assembly) rowCap(c *candRow) int {
	if c.maxItems > 0 && c.maxItems < a.limit {
		return c.maxItems
	}
	return a.limit
}

// newViewer reports whether the viewer's taste is still too thin for a
// personal Home.
func (a *assembly) newViewer() bool {
	return a.model.taste == nil || a.model.taste.Confidence < coldStartConfidence
}

// coldBoost lifts the curated cold-start rows on a new viewer's Home so
// it leads with Top 10 today, genre showcases and new releases.
func coldBoost(kind string) float64 {
	switch kind {
	case KindTop10Today:
		return 1.8
	case KindShowcase:
		return 1.5
	case KindNewReleases:
		return 1.6
	}
	return 1
}

// typeOK reports whether an item belongs on this screen.
func (a *assembly) typeOK(t *core.Title) bool {
	switch a.section {
	case core.TypeMovie:
		if t.Type != core.TypeMovie {
			return false
		}
	case core.TypeShow:
		if t.Type != core.TypeShow && t.Type != core.TypeSeason && t.Type != core.TypeEpisode {
			return false
		}
	}
	if a.allowed != nil {
		typ := t.Type
		if typ == core.TypeSeason || typ == core.TypeEpisode {
			typ = core.TypeShow
		}
		if !a.allowed(typ) {
			return false
		}
	}
	return recommend.ContentRatingAllowed(t.ContentRating, a.user.MaxContentRating)
}

// contextItems filters a row's items for this screen and viewer, scores
// them for the context and orders them (MMR-diversified unless the row's
// order is meaningful).
func (a *assembly) contextItems(c *candRow) []candItem {
	target := a.model.taste.targetMinutes(a.hc)
	src := c.items
	if l, ok := c.typed[a.section]; ok && a.section != "" {
		src = l // type-native list for a Movies / TV Shows page
	}
	if a.section == "" && discoveryPriority(c.kind) < 5 && c.kind != KindContinue && len(c.typed) > 0 {
		// Full type-native pools prevent one type from exhausting the mixed
		// pool before context filtering and the page's deduplication draft.
		src = append(append([]candItem(nil), c.typed[core.TypeMovie]...), c.typed[core.TypeShow]...)
	}
	out := make([]candItem, 0, len(src))
	for _, it := range src {
		if a.hidden[it.t.Key] || !a.typeOK(&it.t) {
			continue
		}
		if c.kind == KindShortOnTime && !isShort(&it.t) {
			continue // the context target orders the row (adjust), isShort bounds it
		}
		if !c.keepOrder {
			it.base += a.adjust(c, &it, target)
		}
		out = append(out, it)
	}
	if c.keepOrder {
		// Ordered engine rows hold the Home's titles back too (a stalled
		// series the Home's Up Next already offers), but never reorder:
		// one too thin without them stays as it is. Rows a collection
		// defines (a franchise, an owner collection) keep their titles.
		if c.collKey == 0 && c.source == core.RowSourceEngine {
			if kept, _, demoted := a.holdBackHomeLead(c, out); !demoted {
				return kept
			}
		}
		return out
	}
	sortItems(out)
	n := a.limit
	if c.maxItems > 0 && c.maxItems < n {
		n = c.maxItems
	}
	// Movies / TV pages hold back what the Home leads with; a row too thin
	// without those titles keeps them, after its own.
	out, held, demoted := a.holdBackHomeLead(c, out)
	// Diversify the head; the tail is slack for cross-row dedupe.
	eligible := append([]candItem(nil), out...)
	if demoted {
		eligible = append(eligible, held...)
	}
	window := min(len(out), 3*n+6)
	if a.section == "" && discoveryPriority(c.kind) < 5 {
		window = len(out)
	}
	out = mmr(out, window, a.dials().lambda())
	if demoted {
		out = append(out, mmr(held, len(held), a.dials().lambda())...)
	}
	if a.section == "" && discoveryPriority(c.kind) < 5 {
		out = mixDiscoveryTypes(out)
	}
	return a.keepPreviouslyShown(c, eligible, out)
}

// adjust is the context-dependent part of an item's score.
func (a *assembly) adjust(c *candRow, it *candItem, target float64) float64 {
	t := &it.t
	adj := 0.0
	// Shown repeatedly without a play: demote so Home never goes stale.
	adj -= 0.12 * a.model.fatigue[t.Key]
	// What this viewer watches in this context (sub-persona mode).
	if gs := normGenres(t.Genres); len(gs) > 0 {
		adj += 0.08 * a.model.taste.ContextGenreLift(a.hc, gs)
	}
	// Runtime fit to the time the viewer has.
	if m := minutes(t); m > 0 && target > 0 {
		fitR := 1.0
		if m > target {
			d := (m - target) / target
			fitR = math.Exp(-2 * d * d)
		}
		switch {
		case c.kind == KindShortOnTime:
			adj += 0.35 * fitR
			// Films or episodes, as this viewer watches in this context.
			share := 2*a.model.taste.movieShareFor(a.hc) - 1
			if t.Type == core.TypeMovie {
				adj += 0.1 * share
			} else {
				adj -= 0.1 * share
			}
		case c.kind == KindTop10:
			adj += 0.08 * fitR
			adj -= 0.04 * a.model.fatigue[t.Key]
		case a.hc.Device == core.DeviceMobile || (!a.hc.Weekend && a.hc.Daypart == core.DaypartLate):
			adj += 0.05 * fitR
		}
	}
	// Top 10 for You: a modest freshness term from trend momentum (buzz.go),
	// 0.04 × buzz at the default Freshness, none at 0, double at 1.
	if c.kind == KindTop10 {
		adj += 0.04 * (2 * a.dials().freshness) * a.model.buzz[tasteKey(t.Key)]
	}
	// What the viewer is browsing tonight (session intent, #32): a
	// bounded lift for matching genres/decades/people. Never a vetoed
	// title; restrictions were already applied by typeOK.
	if a.intent.active() && !genreVetoed(a.model.taste, t) {
		adj += a.intent.boostFor(t)
	}
	// Daily rotation: a small, stable-per-day nudge so near-ties reorder.
	adj += 0.06 * (unit(a.user.ID, a.day, t.Key) - 0.5)
	adj += a.tuneItem(it) // owner's page dials (tuning.go)
	// Learned ranker: bounded model blend + exploration (ranker.go).
	adj += a.rankerItem(c, it, adj)
	return adj
}

// rowScore predicts the row's engagement for this viewer and context:
//
//	score = bandit play rate × context multiplier × content strength
//	        × row scale × daily rotation
func (a *assembly) rowScore(c *candRow, items []candItem) (score, strength float64) {
	rng := seeded("row", a.user.ID, a.day, a.visit, a.hc.Bucket(), c.kind)
	rate := a.banditRate(c, rng) // (surface, kind) arm with kind fallback (surfaces.go)

	strength = 0.8
	if c.kind != KindContinue && c.kind != KindUpNext {
		n := min(5, len(items))
		if n > 0 {
			sum := 0.0
			for _, it := range items[:n] {
				sum += 0.7*it.fit + 0.3*it.qual
			}
			strength = clamp(sum/float64(n), 0, 1)
		}
	}
	if c.kind == KindContinue && len(items) == 0 {
		return 0, 0
	}
	mult := editorialBoost(c.kind) * ctxMult(c.kind, a.hc) * a.typeMix(items) * (0.7 + 0.6*strength)
	mult *= a.surfaceMult(c) // surface priors, dials, Home-lead demotion (surfaces.go)
	if a.newViewer() {
		mult *= coldBoost(c.kind)
	} else {
		switch c.kind {
		case KindTop10:
			mult *= 1.35
		case KindBecause:
			mult *= 1.25
		case KindTop10Today:
			mult *= 0.72
		case KindCrowd:
			mult *= 1.2
		case KindCrowdTrending:
			mult *= 1.15
		case KindTrending:
			// Personal trending (KindCrowdTrending, KindTrendingTaste) ranks
			// above the generic feed for viewers the engine knows.
			mult *= 0.8
		}
	}
	// Learned ranker: bounded row multiplier (ranker.go).
	mult *= a.rankerRow(c, items)
	scale := c.scale
	if scale <= 0 {
		scale = 1
	}
	// Rotation between same-kind rows (which Because-you-watched rows show
	// today) and among close rows.
	rot := 1 + 0.16*(unit("rot", a.user.ID, a.day, a.visit, c.id)-0.5)
	return rate * mult * scale * rot, strength
}

// ctxMult adjusts a row kind for the request context.
func ctxMult(kind string, hc core.HubContext) float64 {
	m := 1.0
	weeknight := !hc.Weekend
	switch kind {
	case KindShortOnTime:
		if weeknight {
			m *= 1.3
		}
		if hc.Device == core.DeviceMobile {
			m *= 1.4
		}
		if weeknight && hc.Daypart == core.DaypartLate {
			m *= 1.15
		}
		if hc.Weekend && hc.Daypart == core.DaypartEvening && hc.Device == core.DeviceTV {
			m *= 0.55
		}
	case KindComfort:
		if hc.Daypart == core.DaypartLate {
			m *= 1.3
		}
		if hc.Weekend && hc.Daypart == core.DaypartMorning {
			m *= 1.15
		}
	case KindNewThisWeek, KindHiddenGems:
		if hc.Weekend {
			m *= 1.15
		}
	case KindDifferent:
		if hc.Weekend {
			m *= 1.2
		}
		if hc.Daypart == core.DaypartLate {
			m *= 0.8
		}
	case KindTop10:
		if hc.Daypart == core.DaypartEvening {
			m *= 1.1
		}
	case KindUpNext:
		if weeknight && (hc.Daypart == core.DaypartEvening || hc.Daypart == core.DaypartLate) {
			m *= 1.15
		}
		if hc.Device == core.DeviceMobile {
			m *= 1.1
		}
	case KindContinue:
		if hc.Device == core.DeviceTV {
			m *= 1.1
		}
	}
	return m
}

// typeMix favours film-heavy rows where this viewer watches films in this
// context (and on weekend-evening TVs), and episode-heavy rows on phones
// and weeknights.
func (a *assembly) typeMix(items []candItem) float64 {
	n := min(10, len(items))
	if n == 0 {
		return 1
	}
	movies := 0
	for _, it := range items[:n] {
		if it.t.Type == core.TypeMovie {
			movies++
		}
	}
	dm := 2*float64(movies)/float64(n) - 1 // -1 all episodes/shows … +1 all films
	s := a.model.taste.movieShareFor(a.hc)
	m := 1 + 0.2*dm*(2*s-1)
	switch {
	case a.hc.Device == core.DeviceTV && a.hc.Weekend && a.hc.Daypart == core.DaypartEvening:
		m *= 1 + 0.25*dm
	case a.hc.Device == core.DeviceMobile:
		m *= 1 - 0.2*dm
	case !a.hc.Weekend && a.hc.Daypart == core.DaypartLate:
		m *= 1 - 0.1*dm
	}
	return m
}

// hubID is the row's Plex hub identifier on this screen.
func (a *assembly) hubID(c *candRow) string {
	if c.hub != nil && c.hub.ID != "" {
		return c.hub.ID
	}
	if c.kind == KindContinue {
		return "home.continue"
	}
	return sectionPrefix(a.section) + "." + c.id
}

func (a *assembly) homeRow(r *rankedRow, items []candItem, more bool) core.HomeRow {
	c := r.c
	prefix := sectionPrefix(a.section)
	title := c.title
	if st := c.sectionTitles[a.section]; st != "" && a.section != "" {
		title = st
	}
	h := core.Hub{ID: r.hubID, Title: title, Context: "hub." + prefix + "." + c.context, Style: "shelf", Promoted: true, More: more, Key: c.collKey}
	if c.hub != nil {
		if c.hub.Context != "" {
			h.Context = c.hub.Context
		}
		if c.hub.Style != "" {
			h.Style = c.hub.Style
		}
		if c.hub.Key != 0 {
			h.Key = c.hub.Key
		}
		h.More = h.More || c.hub.More
	}
	titles := make([]core.Title, len(items))
	reasons := make(map[core.RatingKey]string, len(items))
	for i, it := range items {
		titles[i] = it.t
		if it.reason != "" {
			reasons[it.t.Key] = it.reason
		}
	}
	h.Items = titles
	h.Type = rowType(titles, a.section)
	return core.HomeRow{
		HubID: r.hubID, Title: title, Kind: c.kind, Reason: c.reason, Score: round3(r.score),
		Pinned: r.d.pinned, Source: c.source, Items: titles, ItemReasons: reasons, Hub: h,
	}
}

// rowType is the hub's item type: the common type of its items, or mixed
// ("") on Home.
func rowType(items []core.Title, section core.MediaType) core.MediaType {
	if len(items) == 0 {
		return section
	}
	t := items[0].Type
	for _, it := range items[1:] {
		if it.Type != t {
			if section != "" {
				return section
			}
			return ""
		}
	}
	return t
}

// ---- MMR ------------------------------------------------------------------

// features are the parts of a title MMR compares.
type features struct {
	genres    map[string]bool
	coll      int64
	directors map[int64]bool
	typ       core.MediaType
}

func featuresOf(t *core.Title) features {
	f := features{genres: map[string]bool{}, coll: t.CollectionID, directors: map[int64]bool{}, typ: t.Type}
	for _, g := range normGenres(t.Genres) {
		f.genres[g] = true
	}
	for _, d := range t.Directors {
		if d.TMDB != 0 {
			f.directors[d.TMDB] = true
		}
	}
	return f
}

// similarity is redundancy between two titles, 0..1: genre overlap
// (Jaccard), same franchise, same director.
func similarity(a, b features) float64 {
	inter := 0
	for g := range a.genres {
		if b.genres[g] {
			inter++
		}
	}
	union := len(a.genres) + len(b.genres) - inter
	s := 0.0
	if union > 0 {
		s = 0.6 * float64(inter) / float64(union)
	}
	if a.coll != 0 && a.coll == b.coll {
		s += 0.5
	}
	for d := range a.directors {
		if b.directors[d] {
			s += 0.25
			break
		}
	}
	return math.Min(s, 1)
}

// mmr reorders the best k items by Maximal Marginal Relevance: each pick
// maximises λ·relevance − (1−λ)·max similarity to the items already
// picked, so a row is not one franchise or one genre wall-to-wall.
// Deterministic: ties break by score, then key.
func mmr(items []candItem, k int, lambda float64) []candItem {
	if k <= 0 || len(items) == 0 {
		return nil
	}
	if k > len(items) {
		k = len(items)
	}
	// Consider a bounded window beyond k so diversity can pull in slightly
	// lower-scored items without scanning everything.
	window := min(len(items), 2*k+10)
	cand := append([]candItem(nil), items[:window]...)
	lo, hi := cand[0].base, cand[0].base
	for _, c := range cand {
		lo, hi = math.Min(lo, c.base), math.Max(hi, c.base)
	}
	// Relevance is normalised over at least half the score scale, so a
	// narrow band of near-equal scores does not turn tiny gaps into
	// decisive ones.
	span := math.Max(hi-lo, 0.5)
	feats := make([]features, len(cand))
	for i := range cand {
		feats[i] = featuresOf(&cand[i].t)
	}
	maxSim := make([]float64, len(cand))
	used := make([]bool, len(cand))
	out := make([]candItem, 0, k)
	for len(out) < k {
		best, bestVal := -1, math.Inf(-1)
		for i := range cand {
			if used[i] {
				continue
			}
			v := lambda*(cand[i].base-lo)/span - (1-lambda)*maxSim[i]
			if best < 0 || v > bestVal || (v == bestVal && (cand[i].base > cand[best].base ||
				(cand[i].base == cand[best].base && cand[i].t.Key < cand[best].t.Key))) {
				best, bestVal = i, v
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		out = append(out, cand[best])
		for i := range cand {
			if !used[i] {
				if s := similarity(feats[i], feats[best]); s > maxSim[i] {
					maxSim[i] = s
				}
			}
		}
	}
	return out
}

// A useful first screen balances current releases, broad discovery and taste.
// These are priors: engagement and explicit owner placement still take priority.
func editorialBoost(kind string) float64 {
	switch kind {
	case KindNewReleases:
		return 1.8
	case KindCrowdTrending:
		return 1.7
	case KindTrending:
		return 1.65
	case KindPopular:
		return 1.45
	case KindTop10Today, KindTop10:
		return 1.25
	case KindFranchise:
		return .65
	case KindComfort:
		return .8
	}
	return 1
}
