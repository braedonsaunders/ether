package personalize

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Taste is one viewer's taste model: signed affinities for genres,
// keywords, people, decades, languages and franchises, learned from their
// watch history, plays, ratings and impressions, with owner overrides on
// top. It is deterministic for a given input and clock, and explains
// itself (Fit returns a reason; Profile lists the weights).
type Taste struct {
	UserID     int64
	BuiltAt    time.Time
	Confidence float64 // 0..1

	Genre     map[string]float64 // -1..1
	Keyword   map[string]float64
	Person    map[int64]float64
	People    map[int64]personInfo
	Decade    map[int]float64
	Language  map[string]float64
	Franchise map[int64]float64
	Studio    map[string]float64

	// Evidence is the per-title evidence the model was built from, by
	// descending |weight|.
	Evidence []Evidence
	Habits   core.TasteHabits

	// DaypartGenre is a learned genre lift per daypart (-0.5..0.5): what
	// this viewer watches more of in the morning than at night.
	DaypartGenre map[string]map[string]float64
	// ContextGenre and DeviceGenre disentangle sub-personas across
	// (weekday|weekend × daypart) and device class (TV vs mobile).
	ContextGenre map[string]map[string]float64
	DeviceGenre  map[string]map[string]float64
	// MovieShare is the smoothed share of films (vs episodes) the viewer
	// watches per "weekday|weekend/daypart" bucket.
	MovieShare map[string]float64

	Overrides []core.TasteOverride

	evidenceByKey map[core.RatingKey]*Evidence
}

type personInfo struct {
	name  string
	role  string // director, writer, actor
	thumb string
	best  float64 // strongest single contribution, to pick the role shown
}

// Evidence kinds.
const (
	EvidenceFinished  = "finished"
	EvidenceWatching  = "watching"
	EvidenceAbandoned = "abandoned"
	EvidenceRated     = "rated"
	EvidenceIgnored   = "ignored" // shown repeatedly, never played
)

// Evidence is what one title says about a viewer's taste. Episodes fold
// onto their show.
type Evidence struct {
	Key         core.RatingKey
	Title       core.Title
	Weight      float64 // signed, decayed, clamped to ±2
	Raw         float64 // before decay
	At          time.Time
	Kind        string
	Completions int // finished viewings (movies) or finished episodes (shows)
	Rewatched   bool
	Rating      float64
	Progress    float64
	IgnoredDays int
}

// Tunables of the taste model.
const (
	defaultHalfLife   = 120 * 24 * time.Hour
	maxEvidenceTitles = 150
	abandonFraction   = 0.10 // a play stopped before 10% is an abandon
	stallDays         = 14   // in progress, under 30%, untouched this long: abandoned
)

// family normalisation floors: raw affinity that maps to weight 1.0 when
// it is the strongest in its family. Small histories stay small.
const (
	floorGenre    = 3.0
	floorKeyword  = 2.0
	floorPerson   = 2.0
	floorDecade   = 3.0
	floorLanguage = 3.0
	floorFranch   = 1.5
	floorStudio   = 2.5
)

// tasteKey maps an episode or season key onto its show.
func tasteKey(k core.RatingKey) core.RatingKey {
	switch k.Kind() {
	case core.KindEpisode, core.KindSeason:
		return core.ShowKey(k.TMDB())
	}
	return k
}

// genreAliases splits TMDB's combined TV genres so film and TV taste share
// one vocabulary.
var genreAliases = map[string][]string{
	"Sci-Fi & Fantasy":   {"Science Fiction", "Fantasy"},
	"Action & Adventure": {"Action", "Adventure"},
	"War & Politics":     {"War", "Politics"},
	"Kids":               {"Family", "Kids"},
}

// normGenres expands combined genres and drops duplicates, keeping order.
func normGenres(gs []string) []string {
	out := make([]string, 0, len(gs)+2)
	seen := map[string]bool{}
	add := func(g string) {
		if g != "" && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	for _, g := range gs {
		if al, ok := genreAliases[g]; ok {
			for _, a := range al {
				add(a)
			}
			continue
		}
		add(g)
	}
	return out
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// tasteInput is everything the model is built from. Gathering it is I/O
// (see engine.gatherTaste); building from it is pure.
type tasteInput struct {
	userID    int64
	now       time.Time
	loc       *time.Location
	halfLife  time.Duration
	history   []core.HistoryEntry                 // newest first
	statuses  map[core.RatingKey]core.WatchStatus // by item key
	titles    map[core.RatingKey]core.Title       // by taste key
	plays     []playRecord                        // oldest first
	shownDays map[core.RatingKey]int              // impressions without play, by item key
	overrides []core.TasteOverride
	favorites []FavoriteSignal
}

type evAcc struct {
	at             time.Time
	completedEps   map[core.RatingKey]bool
	completionDays map[int]bool
	started        bool
	maxViewCount   int
	rating         float64
	epRatings      []float64
	progress       float64
	progressAt     time.Time
	abandons       int
}

// buildTaste builds the model.
func buildTaste(in tasteInput) *Taste {
	if in.loc == nil {
		in.loc = time.Local
	}
	if in.halfLife <= 0 {
		in.halfLife = defaultHalfLife
	}
	accs := map[core.RatingKey]*evAcc{}
	get := func(tk core.RatingKey) *evAcc {
		a := accs[tk]
		if a == nil {
			a = &evAcc{completedEps: map[core.RatingKey]bool{}, completionDays: map[int]bool{}}
			accs[tk] = a
		}
		return a
	}
	bump := func(a *evAcc, t time.Time) {
		if t.After(a.at) {
			a.at = t
		}
	}
	for _, h := range in.history {
		tk := tasteKey(h.Key)
		a := get(tk)
		bump(a, h.ViewedAt)
		a.started = true
		if h.Completed {
			if h.Key.Kind() == core.KindEpisode {
				a.completedEps[h.Key] = true
			} else {
				a.completionDays[dayOf(h.ViewedAt, in.loc)] = true
			}
		}
	}
	// Statuses: view counts (scrobbles), progress and ratings.
	skeys := make([]core.RatingKey, 0, len(in.statuses))
	for k := range in.statuses {
		skeys = append(skeys, k)
	}
	sort.Slice(skeys, func(i, j int) bool { return skeys[i] < skeys[j] })
	for _, k := range skeys {
		ws := in.statuses[k]
		if ws.ViewCount == 0 && ws.ViewOffset == 0 && ws.UserRating == 0 {
			continue
		}
		tk := tasteKey(k)
		a := get(tk)
		if ws.ViewCount > a.maxViewCount {
			a.maxViewCount = ws.ViewCount
		}
		if ws.ViewCount > 0 && k.Kind() == core.KindEpisode {
			a.completedEps[k] = true
		}
		if ws.ViewOffset > 0 && ws.Duration > 0 {
			if f := float64(ws.ViewOffset) / float64(ws.Duration); f > a.progress {
				a.progress, a.progressAt = f, ws.LastViewedAt
			}
		}
		if ws.UserRating > 0 {
			if k == tk {
				a.rating = ws.UserRating
			} else {
				a.epRatings = append(a.epRatings, ws.UserRating)
			}
		}
		bump(a, ws.LastViewedAt)
		if a.at.IsZero() {
			bump(a, ws.UpdatedAt)
		}
	}
	// Plays: abandons, cancelled out by any later real viewing.
	for _, p := range in.plays {
		a := get(p.tasteKey)
		bump(a, p.updated)
		switch {
		case p.abandoned:
			a.abandons++
		case p.completed || p.maxFraction >= 0.25:
			a.abandons = 0
		}
	}

	var evs []Evidence
	for tk, a := range accs {
		e := Evidence{Key: tk, At: a.at, Progress: a.progress}
		isShow := tk.Kind() == core.KindShow
		n := len(a.completionDays)
		if a.maxViewCount > n && !isShow {
			n = a.maxViewCount
		}
		if isShow {
			n = len(a.completedEps)
		}
		e.Completions = n
		w := 0.0
		switch {
		case n > 0:
			if isShow {
				w = 0.6 + 0.4*math.Min(1, float64(n)/3)
				if n > 3 {
					w += math.Min(0.1*float64(n-3), 0.8)
				}
				e.Rewatched = a.maxViewCount > 1
			} else {
				w = 1.0 + math.Min(0.3*float64(n-1), 0.6)
				e.Rewatched = n > 1
			}
			e.Kind = EvidenceFinished
		case a.abandons > 0 && a.progress < 0.25:
			w = -0.5
			e.Kind = EvidenceAbandoned
		case a.progress > 0:
			if in.now.Sub(a.progressAt) > stallDays*24*time.Hour && a.progress < 0.3 {
				w = -0.6
				e.Kind = EvidenceAbandoned
			} else {
				w = 0.1 + 0.4*a.progress
				e.Kind = EvidenceWatching
			}
		case a.started:
			w = 0.1
			e.Kind = EvidenceWatching
		}
		rating := a.rating
		if rating == 0 && len(a.epRatings) > 0 {
			sum := 0.0
			for _, r := range a.epRatings {
				sum += r
			}
			rating = sum / float64(len(a.epRatings))
		}
		if rating > 0 {
			e.Rating = rating
			w += (rating - 5) / 5 * 1.2
			if e.Kind == "" {
				e.Kind = EvidenceRated
			}
		}
		if e.Kind == "" {
			continue
		}
		e.Raw = w
		if !a.at.IsZero() {
			if age := in.now.Sub(a.at); age > 0 {
				w *= math.Pow(0.5, float64(age)/float64(in.halfLife))
			}
		}
		e.Weight = clamp(w, -2, 2)
		if e.Weight == 0 {
			continue
		}
		evs = append(evs, e)
	}
	// Favorites are independent positive evidence with a slower decay than viewing.
	for _, favorite := range in.favorites {
		if favorite.Key == 0 {
			continue
		}
		weight := 1.8
		if age := in.now.Sub(favorite.AddedAt); !favorite.AddedAt.IsZero() && age > 0 {
			weight *= math.Pow(0.5, float64(age)/float64(2*in.halfLife))
		}
		evs = append(evs, Evidence{Key: tasteKey(favorite.Key), At: favorite.AddedAt, Raw: 1.8, Weight: weight, Kind: "favorite"})
	}
	// Impressions without a play: weak negative evidence once a title has
	// been shown on three or more days and never started.
	ignored := map[core.RatingKey]int{}
	for k, days := range in.shownDays {
		if tk := tasteKey(k); days > ignored[tk] {
			ignored[tk] = days
		}
	}
	for tk, days := range ignored {
		if _, ok := accs[tk]; ok || days < 3 {
			continue
		}
		w := -0.06 * math.Min(float64(days-2), 5)
		evs = append(evs, Evidence{Key: tk, Weight: w, Raw: w, Kind: EvidenceIgnored, IgnoredDays: days})
	}

	sort.Slice(evs, func(i, j int) bool {
		wi, wj := math.Abs(evs[i].Weight), math.Abs(evs[j].Weight)
		if wi != wj {
			return wi > wj
		}
		return evs[i].Key < evs[j].Key
	})

	ts := &Taste{
		UserID: in.userID, BuiltAt: in.now,
		Genre: map[string]float64{}, Keyword: map[string]float64{}, Person: map[int64]float64{},
		People: map[int64]personInfo{}, Decade: map[int]float64{}, Language: map[string]float64{},
		Franchise: map[int64]float64{}, Studio: map[string]float64{},
		DaypartGenre: map[string]map[string]float64{}, ContextGenre: map[string]map[string]float64{},
		DeviceGenre: map[string]map[string]float64{}, MovieShare: map[string]float64{},
		evidenceByKey: map[core.RatingKey]*Evidence{},
	}
	mass := 0.0
	used := 0
	for _, e := range evs {
		t, ok := in.titles[e.Key]
		if !ok {
			continue
		}
		if used >= maxEvidenceTitles {
			break
		}
		used++
		e.Title = t
		ts.Evidence = append(ts.Evidence, e)
		w := e.Weight
		if e.Kind == EvidenceIgnored {
			w *= 0.5
		} else if w > 0 {
			mass += w
		} else {
			mass += -w * 0.5
		}
		ts.spread(t, w)
	}
	for _, favorite := range in.favorites {
		if favorite.PersonID <= 0 {
			continue
		}
		weight := 1.8
		if age := in.now.Sub(favorite.AddedAt); !favorite.AddedAt.IsZero() && age > 0 {
			weight *= math.Pow(0.5, float64(age)/float64(2*in.halfLife))
		}
		ts.Person[favorite.PersonID] += weight
		mass += weight
	}
	for i := range ts.Evidence {
		e := &ts.Evidence[i]
		// A favorite adds taste but must not erase a separate playback or
		// rating record used by watched-state and post-play decisions.
		if old := ts.evidenceByKey[e.Key]; old == nil || e.Kind != "favorite" {
			ts.evidenceByKey[e.Key] = e
		}
	}
	normalize(ts.Genre, floorGenre)
	normalize(ts.Keyword, floorKeyword)
	normalize(ts.Person, floorPerson)
	normalize(ts.Decade, floorDecade)
	normalize(ts.Language, floorLanguage)
	normalize(ts.Franchise, floorFranch)
	normalize(ts.Studio, floorStudio)
	ts.Confidence = 1 - math.Exp(-mass/5)

	views := viewingsFrom(in)
	ts.Habits = habitsFrom(views, in, ts.Evidence)
	ts.learnContext(views, in.titles)
	ts.learnDeviceContext(in.plays, in.titles)
	ts.applyOverrides(in.overrides)
	return ts
}

// spread distributes a title's weight onto its features.
func (ts *Taste) spread(t core.Title, w float64) {
	for _, g := range normGenres(t.Genres) {
		ts.Genre[g] += w
	}
	for i, kw := range t.Keywords {
		if i >= 15 {
			break
		}
		ts.Keyword[strings.ToLower(kw)] += w * 0.5
	}
	addPerson := func(p core.Person, role string, mult float64) {
		if p.TMDB == 0 {
			return
		}
		c := w * mult
		ts.Person[p.TMDB] += c
		info := ts.People[p.TMDB]
		info.name = p.Name
		if p.ThumbPath != "" {
			info.thumb = p.ThumbPath
		}
		if math.Abs(c) > info.best {
			info.best, info.role = math.Abs(c), role
		}
		ts.People[p.TMDB] = info
	}
	for _, d := range t.Directors {
		addPerson(d, "director", 1.0)
	}
	for i, wr := range t.Writers {
		if i >= 3 {
			break
		}
		addPerson(wr, "writer", 0.6)
	}
	castMult := []float64{0.8, 0.7, 0.6, 0.5, 0.4}
	for i, c := range t.Cast {
		if i >= len(castMult) {
			break
		}
		addPerson(c, "actor", castMult[i])
	}
	for i, cr := range t.Crew {
		if i >= 6 {
			break
		}
		switch strings.ToLower(cr.Role) {
		case "creator", "showrunner":
			addPerson(cr, "creator", 0.9)
		case "cinematographer", "director of photography":
			addPerson(cr, "cinematographer", 0.65)
		case "composer", "original music composer":
			addPerson(cr, "composer", 0.6)
		}
	}
	for i, st := range t.Studios {
		if i >= 2 || st == "" {
			break
		}
		ts.Studio[st] += w * 0.7
	}
	if t.Year > 0 {
		ts.Decade[t.Year/10*10] += w
	}
	if t.Language != "" {
		ts.Language[t.Language] += w
	}
	if t.CollectionID != 0 {
		ts.Franchise[t.CollectionID] += w * 0.8
	}
}

// normalize maps raw affinities into -1..1: divided by the family's
// strongest |raw| (or the floor, whichever is larger).
func normalize[K comparable](m map[K]float64, floor float64) {
	maxAbs := floor
	for _, v := range m {
		maxAbs = math.Max(maxAbs, math.Abs(v))
	}
	for k, v := range m {
		n := clamp(v/maxAbs, -1, 1)
		if math.Abs(n) < 1e-4 {
			delete(m, k)
			continue
		}
		m[k] = n
	}
}

// applyOverrides sets owner corrections on top of the learned weights.
func (ts *Taste) applyOverrides(os []core.TasteOverride) {
	ts.Overrides = append([]core.TasteOverride{}, os...)
	for _, o := range os {
		w := clamp(o.Weight, -1, 1)
		switch o.Kind {
		case "genre":
			name := o.Value
			for g := range ts.Genre {
				if strings.EqualFold(g, o.Value) {
					name = g
				}
			}
			for _, g := range normGenres([]string{name}) {
				ts.Genre[g] = w
			}
		case "keyword":
			ts.Keyword[strings.ToLower(strings.TrimSpace(o.Value))] = w
		case "person":
			id, err := strconv.ParseInt(strings.TrimSpace(o.Value), 10, 64)
			if err != nil {
				// A name: match a known person.
				for pid, info := range ts.People {
					if strings.EqualFold(info.name, o.Value) {
						id = pid
					}
				}
			}
			if id != 0 {
				ts.Person[id] = w
				if _, ok := ts.People[id]; !ok {
					ts.People[id] = personInfo{name: o.Value, role: "person"}
				}
			}
		}
	}
}

// evidenceFor returns the viewer's evidence for a title (by taste key).
func (ts *Taste) evidenceFor(k core.RatingKey) *Evidence {
	if ts == nil {
		return nil
	}
	return ts.evidenceByKey[tasteKey(k)]
}

// TopGenres returns the n strongest positive genres.
func (ts *Taste) TopGenres(n int) []string {
	type gw struct {
		g string
		w float64
	}
	var all []gw
	for g, w := range ts.Genre {
		if w > 0 {
			all = append(all, gw{g, w})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].w != all[j].w {
			return all[i].w > all[j].w
		}
		return all[i].g < all[j].g
	})
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, all[i].g)
	}
	return out
}

// fitPart is one explained component of a fit score.
type fitPart struct {
	value  float64
	reason string
}

// Fit scores how well a title matches the taste, roughly -1..1, and
// explains the strongest positive reason.
//
//	fit = 0.40·genre + 0.15·keywords + 0.25·people + 0.08·decade
//	    + language (±0.07, see languageFit) + 0.05·franchise
//
// A genre weighted -0.6 or below (usually an owner override) vetoes the
// title: its fit is capped at 0 and pushed down by 0.4×that weight.
func (ts *Taste) Fit(t *core.Title) (float64, string) {
	if ts == nil {
		return 0, ""
	}
	var parts []fitPart
	veto := 0.0 // the most disliked genre, when strongly disliked
	if gs := normGenres(t.Genres); len(gs) > 0 {
		sum, minW, best, bestG := 0.0, 0.0, 0.0, ""
		for _, g := range gs {
			w := ts.Genre[g]
			sum += w
			minW = math.Min(minW, w)
			if w > best {
				best, bestG = w, g
			}
		}
		v := sum / float64(len(gs))
		if minW < -0.3 { // a disliked genre spoils a mixed title
			v += minW * 0.5
		}
		if minW <= -0.6 {
			veto = minW
		}
		parts = append(parts, fitPart{0.40 * clamp(v, -1, 1), genreReason(bestG, best)})
	}
	if len(t.Keywords) > 0 && len(ts.Keyword) > 0 {
		sum, best, bestK := 0.0, 0.0, ""
		for _, kw := range t.Keywords {
			kw = strings.ToLower(kw)
			w := ts.Keyword[kw]
			sum += w
			if w > best || (w == best && w > 0 && kw < bestK) {
				best, bestK = w, kw
			}
		}
		v := clamp(sum/math.Sqrt(float64(len(t.Keywords))), -1, 1)
		r := ""
		if best >= 0.3 {
			r = "About " + bestK
		}
		parts = append(parts, fitPart{0.15 * v, r})
	}
	{
		var contribs []fitPart
		add := func(p core.Person, mult float64, verb string) {
			if p.TMDB == 0 {
				return
			}
			if w, ok := ts.Person[p.TMDB]; ok {
				contribs = append(contribs, fitPart{w * mult, verb + " " + p.Name})
			}
		}
		for _, d := range t.Directors {
			add(d, 1, "Directed by")
		}
		for i, wr := range t.Writers {
			if i < 3 {
				add(wr, 0.7, "Written by")
			}
		}
		for i, c := range t.Cast {
			if i < 5 {
				add(c, 0.9-0.1*float64(i), "With")
			}
		}
		for i, cr := range t.Crew {
			if i >= 6 {
				break
			}
			switch strings.ToLower(cr.Role) {
			case "creator", "showrunner":
				add(cr, 0.95, "Created by")
			case "cinematographer", "director of photography":
				add(cr, 0.75, "Shot by")
			case "composer", "original music composer":
				add(cr, 0.70, "Scored by")
			}
		}
		if len(contribs) > 0 {
			sort.SliceStable(contribs, func(i, j int) bool { return contribs[i].value > contribs[j].value })
			v := contribs[0].value
			if len(contribs) > 1 {
				v += 0.5 * contribs[1].value
			}
			// Negative people pull down too.
			for _, c := range contribs[1:] {
				if c.value < 0 {
					v += 0.5 * c.value
				}
			}
			r := ""
			if contribs[0].value >= 0.25 {
				r = contribs[0].reason
			}
			parts = append(parts, fitPart{0.25 * clamp(v, -1, 1), r})
		}
	}
	if t.Year > 0 {
		d := t.Year / 10 * 10
		w := ts.Decade[d]
		r := ""
		if w >= 0.6 {
			r = fmt.Sprintf("From the %ds you favour", d)
		}
		parts = append(parts, fitPart{0.08 * w, r})
	}
	if t.Language != "" {
		parts = append(parts, fitPart{ts.languageFit(t.Language), ""})
	}
	if t.CollectionID != 0 {
		w := ts.Franchise[t.CollectionID]
		r := ""
		if w > 0.2 && t.CollectionName != "" {
			r = "Part of " + t.CollectionName
		}
		parts = append(parts, fitPart{0.05 * w, r})
	}
	fit, best, why := 0.0, 0.0, ""
	for _, p := range parts {
		fit += p.value
		if p.reason != "" && p.value > best {
			best, why = p.value, p.reason
		}
	}
	if best < 0.04 {
		why = ""
	}
	if veto != 0 {
		// A strongly disliked genre ("not into horror") vetoes the title
		// whatever else matches.
		fit = math.Min(fit, 0) + 0.4*veto
		why = ""
	}
	return fit, why
}

// languageFit only carries information when it distinguishes: a viewer who
// watches in several languages gets a bonus for each of them; a viewer who
// watches almost only in one gets a small penalty for languages they never
// watch. Everyone watching English does not make English a match.
func (ts *Taste) languageFit(lang string) float64 {
	w := ts.Language[lang]
	strong, top := 0, 0.0
	for _, v := range ts.Language {
		if v >= 0.3 {
			strong++
		}
		top = math.Max(top, v)
	}
	switch {
	case w < 0:
		return 0.07 * w
	case strong >= 2 && w >= 0.3:
		return 0.07 * w
	case w == 0 && top >= 0.8:
		return -0.05
	}
	return 0
}

func genreReason(g string, w float64) string {
	if g == "" || w < 0.3 {
		return ""
	}
	return "Matches your taste for " + g
}

// Profile is the owner-facing view of the model. imageURL turns a TMDB
// image path into a browser-loadable URL.
func (ts *Taste) Profile(imageURL func(path string, width int) string) *core.TasteProfile {
	p := &core.TasteProfile{
		UserID: ts.UserID, UpdatedAt: ts.BuiltAt, Confidence: round3(ts.Confidence),
		Genres: []core.TasteWeight{}, People: []core.TastePerson{}, Keywords: []core.TasteWeight{},
		Decades: []core.TasteDecade{}, Languages: []core.TasteLanguage{}, Habits: ts.Habits,
		Overrides: append([]core.TasteOverride{}, ts.Overrides...),
	}
	if p.Habits.PeakHours == nil {
		p.Habits.PeakHours = []int{}
	}
	for _, kv := range sortedByWeight(ts.Genre, 25) {
		p.Genres = append(p.Genres, core.TasteWeight{Name: kv.k, Weight: round3(kv.w)})
	}
	for _, kv := range sortedByWeight(ts.Keyword, 30) {
		p.Keywords = append(p.Keywords, core.TasteWeight{Name: kv.k, Weight: round3(kv.w)})
	}
	for _, kv := range sortedByWeight(ts.Person, 30) {
		info := ts.People[kv.k]
		tp := core.TastePerson{ID: kv.k, Name: info.name, Role: info.role, Weight: round3(kv.w)}
		if info.thumb != "" && imageURL != nil {
			tp.Image = imageURL(info.thumb, 185)
		}
		p.People = append(p.People, tp)
	}
	for _, kv := range sortedByWeight(ts.Decade, 12) {
		p.Decades = append(p.Decades, core.TasteDecade{Decade: kv.k, Weight: round3(kv.w)})
	}
	for _, kv := range sortedByWeight(ts.Language, 10) {
		p.Languages = append(p.Languages, core.TasteLanguage{Code: kv.k, Weight: round3(kv.w)})
	}
	return p
}

type kw[K comparable] struct {
	k K
	w float64
}

// sortedByWeight returns up to n entries by descending |weight| (ties by
// key order of fmt).
func sortedByWeight[K comparable](m map[K]float64, n int) []kw[K] {
	out := make([]kw[K], 0, len(m))
	for k, w := range m {
		out = append(out, kw[K]{k, w})
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := math.Abs(out[i].w), math.Abs(out[j].w)
		if ai != aj {
			return ai > aj
		}
		return fmt.Sprint(out[i].k) < fmt.Sprint(out[j].k)
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
