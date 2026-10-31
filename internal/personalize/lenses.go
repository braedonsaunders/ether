package personalize

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// Lens rows: the Movies and TV Shows pages' own rows (kinds.go). Each is
// type-native (drawn from per-type pools, never a mixed list cut by type),
// built only from data the catalogue has, silent when the viewer's
// history does not support it, and titled with why it exists.

// Occasion row ids (also the occasionMult keys).
const (
	occasionDateNight   = "occasion.date-night"
	occasionFamilyNight = "occasion.family-night"
	occasionShort       = "occasion.under-100"
)

// likedOfType is the viewer's positive evidence for titles of one type
// (finished, rated, or well into), strongest first.
func (b *builder) likedOfType(t core.MediaType) []Evidence {
	var out []Evidence
	for _, e := range b.taste.Evidence {
		if baseType(e.Title.Type) != t || e.Weight < 0.25 {
			continue
		}
		if e.Kind == EvidenceFinished || e.Kind == EvidenceRated || (e.Kind == EvidenceWatching && e.Progress >= 0.5) {
			out = append(out, e)
		}
	}
	return out
}

// rankType is rank restricted to one media type.
func (b *builder) rankType(items []core.Title, t core.MediaType, score scorer) []candItem {
	kept := make([]core.Title, 0, len(items))
	for _, x := range items {
		if x.Type == t {
			kept = append(kept, x)
		}
	}
	return b.rank(kept, score)
}

// withWhy appends the taste reason to an item reason.
func withWhy(r, why string) string {
	if why == "" {
		return r
	}
	if r == "" {
		return why
	}
	return r + " · " + why
}

func ratedPhrase(t *core.Title) string {
	if t.Rating <= 0 {
		return ""
	}
	if t.VoteCount > 0 {
		return fmt.Sprintf("Rated %.1f by %s viewers", t.Rating, votesPhrase(t.VoteCount))
	}
	return fmt.Sprintf("Rated %.1f", t.Rating)
}

func typeTag(t core.MediaType) string {
	if t == core.TypeShow {
		return "series-"
	}
	return ""
}

// ---- taste clusters ----------------------------------------------------------------

// genericKeywords say nothing about taste.
var genericKeywords = map[string]bool{
	"based on novel or book": true, "duringcreditsstinger": true, "aftercreditsstinger": true, "woman director": true,
	"sequel": true, "remake": true, "3d": true, "independent film": true, "based on comic": true, "live action remake": true,
	"anime": true, "based on manga": true, "biography": true, "sitcom": true, "miniseries": true, "limited series": true,
	"mini-series": true, "based on young adult novel": true, "shared universe": true, "reboot": true, "prequel": true,
	"spin off": true, "duringcreditstinger": true, "short film": true, "tv movie": true,
}

// cluster is a named slice of the viewer's taste.
type cluster struct {
	genre, other string // other = a second genre (pair) or a keyword
	keyword      bool
	support      int
	score        float64
}

// genTasteClusters builds named rows from the viewer's most distinctive
// genre × theme and genre × genre combinations: what they keep watching,
// measured against how common the combination is in the catalogue, so
// "Thriller + heist" beats "Thriller + murder" for a heist fan.
func (b *builder) genTasteClusters() []*candRow {
	if !b.personal() {
		return nil
	}
	var rows []*candRow
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
		rows = append(rows, b.clustersOfType(typ)...)
	}
	return rows
}

func (b *builder) clustersOfType(typ core.MediaType) []*candRow {
	liked := b.likedOfType(typ)
	if len(liked) < 3 {
		return nil
	}
	type acc struct {
		w float64
		n int
	}
	kw := map[[2]string]*acc{}
	pair := map[[2]string]*acc{}
	add := func(m map[[2]string]*acc, k [2]string, w float64) {
		a := m[k]
		if a == nil {
			a = &acc{}
			m[k] = a
		}
		a.w += w
		a.n++
	}
	for _, e := range liked {
		gs := normGenres(e.Title.Genres)
		for _, g := range gs {
			for i, k := range e.Title.Keywords {
				k = strings.ToLower(strings.TrimSpace(k))
				if i >= 15 || genericKeywords[k] || len(strings.Fields(k)) > 3 || k == "" || namesGenre(k) {
					continue
				}
				add(kw, [2]string{g, k}, e.Weight)
			}
			for _, g2 := range gs {
				if g2 != g && clusterHead(g2, g) == g2 {
					add(pair, [2]string{g2, g}, e.Weight)
				}
			}
		}
	}
	// Catalogue base rates per genre: how common each theme / second
	// genre is among the genre's popular titles.
	base := map[string][]core.Title{}
	baseOf := func(g string) []core.Title {
		if v, ok := base[g]; ok {
			return v
		}
		v := b.browse(core.BrowseQuery{Filter: core.Filter{Type: typ, Genres: []string{g}}, Sort: core.SortPopularity, Desc: true, Limit: 300})
		base[g] = v
		return v
	}
	rate := func(g, other string, isKW bool) float64 {
		items := baseOf(g)
		if len(items) == 0 {
			return 0
		}
		hit := 0
		for i := range items {
			if isKW && hasKeyword(&items[i], other) || !isKW && anyShared(normGenres(items[i].Genres), []string{other}) {
				hit++
			}
		}
		return (float64(hit) + 0.5) / (float64(len(items)) + 1)
	}
	genreSupport := map[string]int{}
	for _, e := range liked {
		for _, g := range normGenres(e.Title.Genres) {
			genreSupport[g]++
		}
	}
	var cands []cluster
	consider := func(m map[[2]string]*acc, isKW bool) {
		for k, a := range m {
			if a.n < 2 || b.taste.Genre[k[0]] <= 0 {
				continue
			}
			p := float64(a.n) / float64(max(genreSupport[k[0]], 1))
			lift := p / math.Max(rate(k[0], k[1], isKW), 1e-3)
			if lift < 1.5 {
				continue // as common in the catalogue as in their viewing
			}
			s := a.w * clamp((lift-1)/2, 0, 1.5)
			if isKW {
				s *= 1.15 // a theme names a cluster better than a genre pair
			}
			cands = append(cands, cluster{genre: k[0], other: k[1], keyword: isKW, support: a.n, score: s})
		}
	}
	consider(kw, true)
	consider(pair, false)
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if cands[i].genre != cands[j].genre {
			return cands[i].genre < cands[j].genre
		}
		return cands[i].other < cands[j].other
	})
	var rows []*candRow
	usedOther := map[string]bool{}
	perGenre := map[string]int{}
	var shown []map[core.RatingKey]bool
	for _, cl := range cands {
		if len(rows) >= 4 {
			break
		}
		if usedOther[cl.other] || perGenre[cl.genre] >= 2 {
			continue
		}
		var src []core.Title
		if cl.keyword {
			src = baseOf(cl.genre)
		} else {
			src = b.browse(core.BrowseQuery{Filter: core.Filter{Type: typ, Genres: []string{cl.genre, cl.other}}, Sort: core.SortPopularity, Desc: true, Limit: 300})
		}
		src = append(src, b.typedPool(typ)...)
		var in []core.Title
		for i := range src {
			t := &src[i]
			gs := normGenres(t.Genres)
			if !anyShared(gs, []string{cl.genre}) {
				continue
			}
			if cl.keyword && hasKeyword(t, cl.other) || !cl.keyword && anyShared(gs, []string{cl.other}) {
				in = append(in, *t)
			}
		}
		name := clusterName(cl, typ)
		items := b.rankType(in, typ, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
			return 0.5*fit + 0.35*quality(t) + 0.15*popularity(t), withWhy(name+" · "+ratedPhrase(t), why)
		})
		if len(items) < 5 {
			continue
		}
		// Distinct rows: skip a cluster that would mostly repeat one
		// already chosen.
		top := map[core.RatingKey]bool{}
		for _, it := range items[:min(10, len(items))] {
			top[it.t.Key] = true
		}
		dup := false
		for _, prev := range shown {
			n := 0
			for k := range top {
				if prev[k] {
					n++
				}
			}
			if 2*n > len(top) {
				dup = true
			}
		}
		if dup {
			continue
		}
		shown = append(shown, top)
		usedOther[cl.other] = true
		perGenre[cl.genre]++
		noun := "films"
		if typ == core.TypeShow {
			noun = "series"
		}
		id := "cluster." + typeTag(typ) + genreSlug(cl.genre) + "." + genreSlug(cl.other)
		r := newRow(KindTasteCluster, id, name,
			fmt.Sprintf("You keep coming back to %s: %d %s you enjoyed. More in that vein you haven't seen.", strings.ToLower(name), cl.support, noun),
			items)
		r.minItems, r.scale = 5, 0.85+0.15*clamp(cl.score, 0, 1)
		rows = append(rows, r)
	}
	return rows
}

// namesGenre reports whether a keyword just restates a genre ("horror",
// "sci-fi"): that is a genre, not a theme.
func namesGenre(k string) bool {
	for g := range genreFilms {
		if strings.Contains(k, strings.ToLower(g)) || strings.Contains(k, genreSlug(g)) {
			return true
		}
	}
	return strings.Contains(k, "sci-fi")
}

func hasKeyword(t *core.Title, k string) bool {
	for _, x := range t.Keywords {
		if strings.EqualFold(strings.TrimSpace(x), k) {
			return true
		}
	}
	return false
}

// clusterHeads orders genres by how well they work as the head noun of a
// cluster name ("Sci-Fi Thrillers", not "Thrilling Sci-Fi").
var clusterHeads = []string{"Comedy", "Drama", "Thriller", "Horror", "Mystery", "Adventure", "Action", "Romance", "Western",
	"War", "Fantasy", "Science Fiction", "Crime", "Family", "Animation", "Documentary", "History", "Music"}

// clusterHead returns which of two genres heads a pair name.
func clusterHead(a, c string) string {
	ia, ic := len(clusterHeads), len(clusterHeads)
	for i, g := range clusterHeads {
		if g == a {
			ia = i
		}
		if g == c {
			ic = i
		}
	}
	if ia < ic || ia == ic && a < c {
		return a
	}
	return c
}

// Genre words for names.
var (
	genreAdjective = map[string]string{
		"Science Fiction": "Sci-Fi", "Romance": "Romantic", "Animation": "Animated", "Crime": "Crime", "Horror": "Horror",
		"Action": "Action", "Fantasy": "Fantasy", "Family": "Family", "Mystery": "Mystery", "Thriller": "Thriller",
		"Drama": "Dramatic", "Comedy": "Comic", "History": "Historical", "War": "War", "Music": "Musical",
		"Documentary": "Documentary", "Adventure": "Adventure", "Western": "Western",
	}
	genreFilms = map[string]string{
		"Comedy": "Comedies", "Drama": "Dramas", "Thriller": "Thrillers", "Horror": "Horror Films", "Mystery": "Mysteries",
		"Adventure": "Adventures", "Action": "Action Films", "Romance": "Romances", "Western": "Westerns", "War": "War Films",
		"Fantasy": "Fantasy Films", "Science Fiction": "Sci-Fi Films", "Crime": "Crime Films", "Family": "Family Films",
		"Animation": "Animated Films", "Documentary": "Documentaries", "History": "History Films", "Music": "Music Films",
	}
	genreSingular = map[string]string{"Science Fiction": "Sci-Fi", "Animation": "Animated"}
)

// genreNoun names a genre's titles of a type: "Thrillers" / "Thriller
// Series".
func genreNoun(g string, t core.MediaType) string {
	if t == core.TypeShow {
		s := genreSingular[g]
		if s == "" {
			s = g
		}
		return s + " Series"
	}
	if n := genreFilms[g]; n != "" {
		return n
	}
	return g + " Films"
}

// themeWords turns a keyword into a compound modifier: "time travel" →
// "Time-Travel", "coming of age" → "Coming-of-Age".
func themeWords(k string) string {
	words := strings.Fields(k)
	for i, w := range words {
		switch {
		case i > 0 && (w == "of" || w == "the" || w == "and" || w == "in" || w == "on" || w == "a"):
		default:
			// Upper-case the first rune, not the first byte: TMDB keywords
			// can start with a multi-byte letter ("émigré", "à").
			r, n := utf8.DecodeRuneInString(w)
			words[i] = string(unicode.ToUpper(r)) + w[n:]
		}
	}
	return strings.Join(words, "-")
}

// clusterName names a cluster: "Heist Thrillers", "Sci-Fi Thrillers",
// "Time-Travel Sci-Fi Series".
func clusterName(cl cluster, t core.MediaType) string {
	if cl.keyword {
		return themeWords(cl.other) + " " + genreNoun(cl.genre, t)
	}
	mod := genreAdjective[cl.other]
	if mod == "" {
		mod = cl.other
	}
	head := genreNoun(cl.genre, t)
	if cl.genre == "Family" && cl.other == "Animation" && t == core.TypeMovie {
		return "Animated Family Films"
	}
	return mod + " " + head
}

// ---- era ---------------------------------------------------------------------------

// genEra finds the decades the viewer's finished films cluster in and
// offers that era's best unseen films.
func (b *builder) genEra() []*candRow {
	if !b.personal() {
		return nil
	}
	count := map[int]int{}
	for _, e := range b.likedOfType(core.TypeMovie) {
		if e.Title.Year > 0 {
			count[e.Title.Year/10*10]++
		}
	}
	current := b.now.Year() / 10 * 10
	var decades []int
	for d, n := range count {
		if n >= 2 && d < current && b.taste.Decade[d] >= 0.35 {
			decades = append(decades, d)
		}
	}
	sort.Slice(decades, func(i, j int) bool {
		if b.taste.Decade[decades[i]] != b.taste.Decade[decades[j]] {
			return b.taste.Decade[decades[i]] > b.taste.Decade[decades[j]]
		}
		return decades[i] > decades[j]
	})
	var rows []*candRow
	for _, d := range decades {
		if len(rows) >= 2 {
			break
		}
		f := core.Filter{Type: core.TypeMovie, YearFrom: d, YearTo: d + 9}
		src := b.browse(core.BrowseQuery{Filter: f, Sort: core.SortPopularity, Desc: true, Limit: 300})
		f.MinRating = 7
		src = append(src, b.browse(core.BrowseQuery{Filter: f, Sort: core.SortRating, Desc: true, Limit: 100})...)
		var in []core.Title
		for _, t := range src {
			if t.Year >= d && t.Year <= d+9 && weightedRating(&t) >= 6.5 {
				in = append(in, t)
			}
		}
		items := b.rankType(in, core.TypeMovie, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
			return 0.5*fit + 0.35*quality(t) + 0.15*popularity(t), withWhy(fmt.Sprintf("%d · %s", t.Year, ratedPhrase(t)), why)
		})
		if len(items) < 5 {
			continue
		}
		r := newRow(KindEra, fmt.Sprintf("era.%d", d), "Best of the "+decadeName(d)+" for You",
			fmt.Sprintf("You've enjoyed %d films from the %ds; these are that decade's best you haven't seen.", count[d], d), items)
		r.minItems = 5
		r.scale = 0.85 + 0.15*b.taste.Decade[d]
		rows = append(rows, r)
	}
	return rows
}

// decadeName: 1990 → "'90s", 2010 → "2010s".
func decadeName(d int) string {
	if d >= 1900 && d < 2000 {
		return fmt.Sprintf("'%02ds", d%100)
	}
	return fmt.Sprintf("%ds", d)
}

// ---- world cinema ------------------------------------------------------------------

var languageNames = map[string]string{
	"ko": "Korean", "ja": "Japanese", "fr": "French", "es": "Spanish", "de": "German", "it": "Italian", "hi": "Hindi",
	"zh": "Chinese", "cn": "Cantonese", "da": "Danish", "sv": "Swedish", "no": "Norwegian", "pt": "Portuguese", "ru": "Russian",
	"tr": "Turkish", "th": "Thai", "fa": "Persian", "pl": "Polish", "nl": "Dutch", "ta": "Tamil", "te": "Telugu",
	"ml": "Malayalam", "id": "Indonesian", "ar": "Arabic", "en": "English", "fi": "Finnish", "he": "Hebrew", "el": "Greek",
	"hu": "Hungarian", "cs": "Czech", "ro": "Romanian", "is": "Icelandic", "tl": "Filipino", "vi": "Vietnamese",
}

// genWorld offers films in the languages the viewer's history shows they
// watch beyond their main one, or world cinema across several.
func (b *builder) genWorld() []*candRow {
	if !b.personal() {
		return nil
	}
	count := map[string]int{}
	for _, e := range b.likedOfType(core.TypeMovie) {
		if e.Title.Language != "" {
			count[e.Title.Language]++
		}
	}
	main, mainN := "", 0
	for l, n := range count {
		if n > mainN || n == mainN && l < main {
			main, mainN = l, n
		}
	}
	var langs []string
	other := 0
	for l, n := range count {
		if l == main {
			continue
		}
		other += n
		if n >= 2 && b.taste.Language[l] >= 0.2 && languageNames[l] != "" {
			langs = append(langs, l)
		}
	}
	sort.Slice(langs, func(i, j int) bool {
		if count[langs[i]] != count[langs[j]] {
			return count[langs[i]] > count[langs[j]]
		}
		return langs[i] < langs[j]
	})
	var rows []*candRow
	build := func(id, title, reason string, filterLangs []string, keep func(*core.Title) bool) {
		var src []core.Title
		for _, l := range filterLangs {
			src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, Languages: []string{l}}, Sort: core.SortPopularity, Desc: true, Limit: 200})...)
		}
		if len(filterLangs) == 0 {
			src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, MinRating: 7}, Sort: core.SortPopularity, Desc: true, Limit: 400})...)
		}
		var in []core.Title
		for i := range src {
			if keep(&src[i]) {
				in = append(in, src[i])
			}
		}
		items := b.rankType(in, core.TypeMovie, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
			lang := languageNames[t.Language]
			r := ratedPhrase(t)
			if lang != "" {
				r = lang + " · " + r
			}
			return 0.5*fit + 0.4*quality(t) + 0.1*popularity(t), withWhy(r, why)
		})
		if len(items) < 4 {
			return
		}
		r := newRow(KindWorld, id, title, reason, items)
		rows = append(rows, r)
	}
	for _, l := range langs {
		if len(rows) >= 2 {
			break
		}
		name := languageNames[l]
		build("world."+l, name+" Cinema for You", fmt.Sprintf("You've enjoyed %d %s films; more of the best you haven't seen.", count[l], name),
			[]string{l}, func(t *core.Title) bool { return t.Language == l })
	}
	if len(rows) == 0 && other >= 3 && main != "" {
		build("world.all", "World Cinema for You",
			fmt.Sprintf("You've enjoyed %d films beyond %s; acclaimed world cinema picked for your taste.", other, languageOrCode(main)),
			nil, func(t *core.Title) bool { return t.Language != "" && t.Language != main })
	}
	return rows
}

func languageOrCode(l string) string {
	if n := languageNames[l]; n != "" {
		return n
	}
	return l
}

// ---- occasions ---------------------------------------------------------------------

// genOccasions builds Date Night and Family Night when the viewer's
// history supports them, and Under 100 Minutes.
func (b *builder) genOccasions() []*candRow {
	if !b.personal() {
		return nil
	}
	liked := b.likedOfType(core.TypeMovie)
	countGenre := func(gs ...string) int {
		n := 0
		for _, e := range liked {
			if anyShared(normGenres(e.Title.Genres), gs) {
				n++
			}
		}
		return n
	}
	// Family films the viewer finished that a family can watch (not an
	// R-rated animated horror).
	familyFilms := 0
	for _, e := range liked {
		if anyShared(normGenres(e.Title.Genres), []string{"Family", "Animation"}) && e.Title.ContentRating != "" &&
			contentrating.Allowed(e.Title.ContentRating, "PG") {
			familyFilms++
		}
	}
	var rows []*candRow
	occasion := func(id, title, reason string, src []core.Title, keep func(*core.Title) bool, why func(*core.Title) string) {
		var in []core.Title
		for i := range src {
			if keep(&src[i]) {
				in = append(in, src[i])
			}
		}
		items := b.rankType(in, core.TypeMovie, func(t *core.Title, _, _ int, fit float64, tw string) (float64, string) {
			return 0.5*fit + 0.35*quality(t) + 0.15*popularity(t), withWhy(why(t), tw)
		})
		if len(items) < 5 {
			return
		}
		r := newRow(KindOccasion, id, title, reason, items)
		r.minItems = 5
		rows = append(rows, r)
	}
	runtimeOK := func(t *core.Title, lo, hi float64) bool {
		m := minutes(t)
		return m == 0 || m >= lo && m <= hi
	}
	if n := countGenre("Romance"); n >= 2 && b.taste.Genre["Romance"] >= 0.25 {
		var src []core.Title
		for _, g := range []string{"Romance", "Comedy"} {
			src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, Genres: []string{g}}, Sort: core.SortPopularity, Desc: true, Limit: 200})...)
		}
		occasion(occasionDateNight, "Date Night", fmt.Sprintf("You've enjoyed %d romances: well-loved picks for two, at a date-night length.", n), src,
			func(t *core.Title) bool {
				gs := normGenres(t.Genres)
				return anyShared(gs, []string{"Romance"}) && !anyShared(gs, []string{"Animation", "Family", "Horror", "Documentary"}) &&
					runtimeOK(t, 85, 135) && weightedRating(t) >= 6.5
			},
			func(t *core.Title) string { return "Romance · " + runtimePhrase(t) })
	}
	if n := familyFilms; n >= 2 && math.Max(b.taste.Genre["Family"], b.taste.Genre["Animation"]) >= 0.25 {
		var src []core.Title
		for _, g := range []string{"Family", "Animation"} {
			src = append(src, b.browse(core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, Genres: []string{g}}, Sort: core.SortPopularity, Desc: true, Limit: 200})...)
		}
		occasion(occasionFamilyNight, "Family Night", fmt.Sprintf("You've enjoyed %d family and animated films: picks everyone can watch together.", n), src,
			func(t *core.Title) bool {
				return anyShared(normGenres(t.Genres), []string{"Family", "Animation"}) && t.ContentRating != "" &&
					contentrating.Allowed(t.ContentRating, "PG") && weightedRating(t) >= 6.3
			},
			func(t *core.Title) string { return t.ContentRating + " · " + runtimePhrase(t) })
	}
	// Under 100 Minutes when the viewer favours shorter films or has short
	// weeknight sessions.
	short, known := 0, 0
	for _, e := range liked {
		if m := minutes(&e.Title); m > 0 {
			known++
			if m <= shortMovieMinutes {
				short++
			}
		}
	}
	reason := ""
	switch wn := b.taste.Habits.WeeknightMinutes; {
	case known >= 3 && 5*short >= 2*known:
		reason = fmt.Sprintf("%d of the %d films you finished ran under 100 minutes: more that fit a shorter evening, picked for your taste.", short, known)
	case wn > 0 && wn <= 110:
		reason = fmt.Sprintf("Your weeknight sessions run about %d minutes: films that fit, picked for your taste.", int(wn))
	}
	if reason != "" {
		src := append(b.browse(core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, MaxRuntime: shortMovieMinutes * time.Minute}, Sort: core.SortPopularity, Desc: true, Limit: 200}),
			b.typedPool(core.TypeMovie)...)
		occasion(occasionShort, "Under 100 Minutes", reason, src,
			func(t *core.Title) bool {
				m := minutes(t)
				return m > 0 && m <= shortMovieMinutes && weightedRating(t) >= 6.3
			},
			func(t *core.Title) string { return runtimePhrase(t) })
	}
	return rows
}

// runtimePhrase: "1h 52m", "94 min".
func runtimePhrase(t *core.Title) string {
	m := int(minutes(t))
	switch {
	case m <= 0:
		return ""
	case m < 100:
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

// ---- acclaimed and top rated ---------------------------------------------------------

// genAcclaimed: films with a trustworthy high rating the viewer hasn't
// seen, ranked by fit (Top Rated ranks by rating alone).
func (b *builder) genAcclaimed() []*candRow {
	if !b.personal() {
		return nil
	}
	src := append(b.list("top_rated", core.TypeMovie, 100),
		b.browse(core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, MinRating: 7}, Sort: core.SortPopularity, Desc: true, Limit: 400})...)
	src = append(src, b.typedPool(core.TypeMovie)...)
	var good []core.Title
	for i := range src {
		if b.acclaimed(&src[i], 1.5, 7.3) {
			good = append(good, src[i])
		}
	}
	items := b.rankType(good, core.TypeMovie, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
		return 0.55*fit + 0.35*quality(t) + 0.1*popularity(t), withWhy(ratedPhrase(t), why)
	})
	kept := items[:0:0]
	for _, it := range items {
		if it.fit >= 0.05 {
			kept = append(kept, it)
		}
	}
	r := newRow(KindAcclaimed, "acclaimed", "Acclaimed & Unseen",
		"Films with a trustworthy high rating you haven't seen, ranked by how well they fit you.", kept)
	r.minItems = 5
	return []*candRow{r}
}

// genTopGenre: the best-rated titles in each of the viewer's favourite
// genres of each type.
func (b *builder) genTopGenre() []*candRow {
	if !b.personal() {
		return nil
	}
	var rows []*candRow
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeShow} {
		weight := map[string]float64{}
		for _, e := range b.likedOfType(typ) {
			for _, g := range normGenres(e.Title.Genres) {
				weight[g] += e.Weight
			}
		}
		genres := make([]string, 0, len(weight))
		for g := range weight {
			if b.taste.Genre[g] >= 0.3 {
				genres = append(genres, g)
			}
		}
		sort.Slice(genres, func(i, j int) bool {
			if weight[genres[i]] != weight[genres[j]] {
				return weight[genres[i]] > weight[genres[j]]
			}
			return genres[i] < genres[j]
		})
		n := 0
		for _, g := range genres {
			if n >= 3 {
				break
			}
			src := b.browse(core.BrowseQuery{Filter: core.Filter{Type: typ, Genres: []string{g}, MinRating: 7}, Sort: core.SortPopularity, Desc: true, Limit: 300})
			var good []core.Title
			for i := range src {
				if b.acclaimed(&src[i], 1, 7.2) {
					good = append(good, src[i])
				}
			}
			items := b.rankType(good, typ, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
				return 0.75*quality(t) + 0.25*fit, withWhy(ratedPhrase(t), why)
			})
			if len(items) < 5 {
				continue
			}
			noun := genreNoun(g, typ)
			r := newRow(KindTopGenre, "toprated.genre."+typeTag(typ)+genreSlug(g), "Top-Rated "+noun,
				fmt.Sprintf("The best-rated %s with enough votes to trust, in one of your favourite genres.", strings.ToLower(noun)), items)
			r.minItems = 5
			rows = append(rows, r)
			n++
		}
	}
	return rows
}

// genStudio builds "More from <Studio>" film shelves when the viewer has
// finished at least two films from a studio or boutique label (A24, Neon,
// Studio Ghibli, etc.).
func (b *builder) genStudio() []*candRow {
	if !b.personal() {
		return nil
	}
	weight := map[string]float64{}
	films := map[string]int{}
	for _, e := range b.likedOfType(core.TypeMovie) {
		for i, s := range e.Title.Studios {
			if i >= 2 || s == "" {
				break
			}
			weight[s] += e.Weight
			films[s]++
		}
	}
	var studios []string
	for s, n := range films {
		if n >= 2 && b.taste.Studio[s] >= 0.25 {
			studios = append(studios, s)
		}
	}
	sort.Slice(studios, func(i, j int) bool {
		if weight[studios[i]] != weight[studios[j]] {
			return weight[studios[i]] > weight[studios[j]]
		}
		return studios[i] < studios[j]
	})
	var rows []*candRow
	for _, s := range studios {
		if len(rows) >= 3 {
			break
		}
		src := b.browse(core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, Studio: s}, Sort: core.SortPopularity, Desc: true, Limit: 100})
		var in []core.Title
		for i := range src {
			for _, x := range src[i].Studios {
				if strings.EqualFold(x, s) {
					in = append(in, src[i])
					break
				}
			}
		}
		studio := s
		items := b.rankType(in, core.TypeMovie, func(t *core.Title, _, _ int, fit float64, why string) (float64, string) {
			return 0.55*fit + 0.3*quality(t) + 0.15*popularity(t), withWhy("From "+studio, why)
		})
		if len(items) < 4 {
			continue
		}
		r := newRow(KindStudio, "studio."+genreSlug(s), "More from "+s,
			fmt.Sprintf("You've enjoyed %d %s films; more from them you haven't seen.", films[s], s), items)
		r.minItems = 4
		r.scale = 0.85 + 0.15*clamp(weight[s]/3, 0, 1)
		rows = append(rows, r)
	}
	return rows
}

// anyShared reports whether two string lists share an element.
func anyShared(a, c []string) bool {
	for _, x := range a {
		for _, y := range c {
			if x == y {
				return true
			}
		}
	}
	return false
}
