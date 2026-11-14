package collections

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
)

// ParseVibe turns a natural-language "vibe" query ("90s thrillers under
// 100 mins", "korean horror", "shows like the office") into a structured
// filter plus a human title. It is a deterministic, conservative
// grammar: every word of the query must be consumed by a known pattern
// (genre, decade/year, runtime, rating, language, media type, mood,
// person/studio shape or a "like X" seed), otherwise ok is false and
// the query is treated as a plain title search. No I/O: people and
// "like" seeds come back unresolved (PersonName / SimilarSeed) for the
// caller to look up.
func ParseVibe(query string) (core.VibeQuery, bool) {
	q := strings.TrimSpace(query)
	if q == "" || len(q) > 300 {
		return core.VibeQuery{}, false
	}
	// Explicit subject phrases must win over the "<person> movies" shape.
	for _, prefix := range []string{"movies about ", "films about ", "shows about ", "series about ", "about "} {
		if strings.HasPrefix(strings.ToLower(q), prefix) {
			return parseTopicVibe(q)
		}
	}
	// "shows like the office" / "movies similar to heat" / "like dark".
	if v, ok := parseSimilar(q); ok {
		return v, true
	}
	// "films by christopher nolan", "directed by ...", "starring ...".
	if v, ok := parseByPerson(q); ok {
		return v, true
	}
	p := &vibeParser{tokens: vibeTokens(q), sort: core.SortPopularity, desc: true}
	if !p.run() {
		return core.VibeQuery{}, false
	}
	// "<name> films/movies/shows": whatever words survive, when the
	// query also names a media type, are a person (or studio) name.
	if len(p.left) > 0 {
		if p.media == "" || p.signals > 0 || len(p.left) > 4 {
			return core.VibeQuery{}, false
		}
		name := strings.Join(p.left, " ")
		if studio, disp, ok := knownStudio(name); ok {
			p.filter.Studio = studio
			p.studioDisp = disp
			p.signals++
			p.left = nil
		} else if len(p.left) >= 2 {
			p.person = name
			p.signals++
			p.left = nil
		} else {
			return core.VibeQuery{}, false
		}
	}
	if p.signals == 0 {
		return core.VibeQuery{}, false
	}
	p.filter.Type = p.media
	v := core.VibeQuery{
		Query: q, Filter: p.filter, Sort: p.sort, Desc: p.desc,
		PersonName: p.person,
	}
	v.Title = p.title()
	return v, true
}

// vibeTokens lower-cases and splits a query, keeping comparison symbols
// and hyphens inside words ("feel-good", "sci-fi").
func vibeTokens(q string) []string {
	q = strings.ToLower(q)
	q = strings.Map(func(r rune) rune {
		if r == '\'' || r == '’' {
			return -1 // "90's" -> "90s"
		}
		return r
	}, q)
	fields := strings.FieldsFunc(q, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == '"' || r == '(' || r == ')'
	})
	out := fields[:0]
	for _, f := range fields {
		f = strings.Trim(f, ".,!?")
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

type vibeParser struct {
	tokens []string
	left   []string // unconsumed tokens, in order

	filter  core.Filter
	media   core.MediaType
	sort    core.SortOrder
	desc    bool
	signals int
	person  string

	// title parts
	decadePart string
	yearPart   string
	langPart   string
	moodPart   string
	genreDisp  string // plural noun ("Thrillers") when known
	genreAdj   string // adjective form ("Horror") otherwise
	ratingPart string
	runPart    string
	studioDisp string
	recent     bool
}

// run consumes tokens left to right; unconsumed ones land in left.
func (p *vibeParser) run() bool {
	toks := p.tokens
	for i := 0; i < len(toks); {
		n := p.consume(toks[i:])
		if n == 0 {
			p.left = append(p.left, toks[i])
			i++
			continue
		}
		i += n
	}
	return true
}

var vibeFillers = map[string]bool{
	"a": true, "an": true, "the": true, "some": true, "any": true,
	"me": true, "find": true, "show": true, "give": true, "watch": true,
	"to": true, "for": true, "of": true, "with": true, "and": true,
	"or": true, "please": true, "i": true, "want": true, "looking": true,
	"good": true, "from": true, "in": true, "on": true,
}

// consume tries every pattern against the head tokens and returns how
// many it used (0 = no pattern matched).
func (p *vibeParser) consume(t []string) int {
	if n := p.consumeYear(t); n > 0 {
		return n
	}
	if n := p.consumeRuntime(t); n > 0 {
		return n
	}
	if n := p.consumeRating(t); n > 0 {
		return n
	}
	if n := p.consumePhrase(t); n > 0 {
		return n
	}
	w := t[0]
	switch {
	case vibeFillers[w]:
		return 1
	case w == "movie" || w == "movies" || w == "film" || w == "films" || w == "flicks":
		p.media = core.TypeMovie
		return 1
	case w == "show" || w == "shows" || w == "series" || w == "tv":
		p.media = core.TypeShow
		return 1
	case w == "short":
		if p.filter.MaxRuntime == 0 && p.filter.MinRuntime == 0 {
			p.filter.MaxRuntime = 100 * time.Minute
			p.runPart = "Under 100 Minutes"
			p.signals++
		}
		return 1
	case w == "long":
		// Also the tail of "100 minutes long", already consumed.
		if p.filter.MaxRuntime == 0 && p.filter.MinRuntime == 0 {
			p.filter.MinRuntime = 140 * time.Minute
			p.runPart = "Over 140 Minutes"
			p.signals++
		}
		return 1
	case w == "modern":
		p.filter.YearFrom = 2000
		p.yearPart = "Modern"
		p.signals++
		return 1
	case w == "recent" || w == "latest" || w == "new" || w == "newest":
		p.recent, p.sort, p.desc = true, core.SortReleased, true
		p.signals++
		return 1
	case w == "classic" || w == "classics" || w == "old":
		if p.filter.YearTo == 0 {
			p.filter.YearTo = 1989
		}
		p.yearPart = "Classic"
		p.signals++
		return 1
	}
	if lg, ok := vibeLanguages[w]; ok {
		p.filter.Languages = appendUniq(p.filter.Languages, lg.lang)
		p.filter.Countries = appendUniq(p.filter.Countries, lg.countries...)
		if p.langPart == "" {
			p.langPart = lg.display
		}
		p.signals++
		return 1
	}
	if g, ok := vibeGenres[w]; ok {
		p.addGenre(g)
		return 1
	}
	if studio, disp, ok := knownStudio(w); ok && p.filter.Studio == "" {
		// A bare studio name only counts next to a media word, handled
		// by the leftover pass; a single token here would swallow
		// title searches like "neon" — so don't consume it here.
		_ = studio
		_ = disp
	}
	return 0
}

func (p *vibeParser) addGenre(g vibeGenre) {
	p.filter.Genres = appendUniq(p.filter.Genres, g.genres...)
	if g.lang != "" {
		p.filter.Languages = appendUniq(p.filter.Languages, g.lang)
	}
	if p.genreDisp == "" {
		p.genreDisp = g.plural
	}
	if p.genreAdj == "" {
		p.genreAdj = g.adj
	}
	if g.mood != "" && p.moodPart == "" {
		p.moodPart = g.mood
	}
	p.signals++
}

// consumePhrase handles multi-word genres and moods.
func (p *vibeParser) consumePhrase(t []string) int {
	join := func(n int) string {
		if len(t) < n {
			return ""
		}
		return strings.Join(t[:n], " ")
	}
	for _, n := range []int{3, 2} {
		if g, ok := vibePhrases[join(n)]; ok {
			p.addGenre(g)
			return n
		}
	}
	// Hyphenated forms collapsed to one token are in vibeGenres; also
	// accept the two-word spellings of the same keys.
	if len(t) >= 2 {
		if g, ok := vibeGenres[t[0]+"-"+t[1]]; ok {
			p.addGenre(g)
			return 2
		}
	}
	return 0
}

var (
	decade4Re = regexp.MustCompile(`^(19\d0|20[0-4]0)s$`)
	decade2Re = regexp.MustCompile(`^’?(\d0)s$`)
	yearRe    = regexp.MustCompile(`^(19\d\d|20[0-4]\d)$`)
)

func (p *vibeParser) setDecade(start int, label string) {
	p.filter.YearFrom, p.filter.YearTo = start, start+9
	p.decadePart = label
	p.signals++
}

// consumeYear handles decades, single years and from/before/between.
func (p *vibeParser) consumeYear(t []string) int {
	w := t[0]
	if strings.Contains(w, "-") { // 1990-1999, 2010s handled below
		a, b, ok := strings.Cut(w, "-")
		if ok && yearRe.MatchString(a) && yearRe.MatchString(b) {
			lo, _ := strconv.Atoi(a)
			hi, _ := strconv.Atoi(b)
			if lo <= hi {
				p.filter.YearFrom, p.filter.YearTo = lo, hi
				p.yearPart = a + "–" + b
				p.signals++
				return 1
			}
		}
	}
	if decade4Re.MatchString(w) {
		start, _ := strconv.Atoi(w[:4])
		p.setDecade(start, w)
		return 1
	}
	if m := decade2Re.FindStringSubmatch(w); m != nil {
		d, _ := strconv.Atoi(m[1])
		start := 1900 + d
		if d <= 20 {
			start = 2000 + d
		}
		p.setDecade(start, m[1]+"s")
		return 1
	}
	if wd, ok := wordDecades[w]; ok {
		p.setDecade(wd.start, wd.label)
		return 1
	}
	if (w == "from" || w == "after" || w == "since") && len(t) > 1 {
		if decade4Re.MatchString(t[1]) {
			start, _ := strconv.Atoi(t[1][:4])
			p.setDecade(start, t[1])
			return 2
		}
		if yearRe.MatchString(t[1]) {
			y, _ := strconv.Atoi(t[1])
			if w == "after" {
				y++
			}
			p.filter.YearFrom = y
			p.yearPart = "From " + t[1]
			p.signals++
			return 2
		}
	}
	if (w == "before" || w == "until") && len(t) > 1 && yearRe.MatchString(t[1]) {
		y, _ := strconv.Atoi(t[1])
		p.filter.YearTo = y - 1
		p.yearPart = "Before " + t[1]
		p.signals++
		return 2
	}
	if w == "between" && len(t) > 3 && yearRe.MatchString(t[1]) && t[2] == "and" && yearRe.MatchString(t[3]) {
		lo, _ := strconv.Atoi(t[1])
		hi, _ := strconv.Atoi(t[3])
		p.filter.YearFrom, p.filter.YearTo = lo, hi
		p.yearPart = t[1] + "–" + t[3]
		p.signals++
		return 4
	}
	if yearRe.MatchString(w) {
		y, _ := strconv.Atoi(w)
		// "1990 to 1999"
		if len(t) > 2 && t[1] == "to" && yearRe.MatchString(t[2]) {
			hi, _ := strconv.Atoi(t[2])
			p.filter.YearFrom, p.filter.YearTo = y, hi
			p.yearPart = w + "–" + t[2]
			p.signals++
			return 3
		}
		p.filter.YearFrom, p.filter.YearTo = y, y
		p.yearPart = w
		p.signals++
		return 1
	}
	return 0
}

func appendUniq(in []string, vs ...string) []string {
	for _, v := range vs {
		found := false
		for _, e := range in {
			if e == v {
				found = true
				break
			}
		}
		if !found {
			in = append(in, v)
		}
	}
	return in
}

// parseRuntimeValue reads "100", "100mins", "2h", "2" + unit tokens and
// returns the duration and tokens used beyond the value token.
func parseRuntimeValue(t []string) (time.Duration, int, bool) {
	if len(t) == 0 {
		return 0, 0, false
	}
	w := t[0]
	// glued forms: 100mins, 90min, 2h, 2hrs, 100m
	for _, suf := range []struct {
		s    string
		unit time.Duration
	}{
		{"minutes", time.Minute}, {"minute", time.Minute}, {"mins", time.Minute},
		{"min", time.Minute}, {"hours", time.Hour}, {"hour", time.Hour},
		{"hrs", time.Hour}, {"hr", time.Hour}, {"h", time.Hour}, {"m", time.Minute},
	} {
		if strings.HasSuffix(w, suf.s) && len(w) > len(suf.s) {
			if n, err := strconv.ParseFloat(w[:len(w)-len(suf.s)], 64); err == nil && n > 0 {
				return time.Duration(n * float64(suf.unit)), 0, true
			}
		}
	}
	n, err := strconv.ParseFloat(w, 64)
	if err != nil || n <= 0 || len(t) < 2 {
		return 0, 0, false
	}
	switch t[1] {
	case "min", "mins", "minute", "minutes", "m":
		return time.Duration(n * float64(time.Minute)), 1, true
	case "h", "hr", "hrs", "hour", "hours":
		return time.Duration(n * float64(time.Hour)), 1, true
	}
	return 0, 0, false
}

// consumeRuntime handles "under 100 mins", "< 2 hours", "over 90 min".
func (p *vibeParser) consumeRuntime(t []string) int {
	w := t[0]
	under := map[string]int{"under": 1, "below": 1, "<": 1, "less": 0, "at": 0}
	over := map[string]int{"over": 1, "above": 1, ">": 1, "more": 0, "at": 0}
	isUnder, isOver := false, false
	skip := 0
	switch {
	case w == "less" && len(t) > 1 && t[1] == "than":
		isUnder, skip = true, 2
	case w == "more" && len(t) > 1 && t[1] == "than":
		isOver, skip = true, 2
	case w == "at" && len(t) > 1 && t[1] == "most":
		isUnder, skip = true, 2
	case w == "at" && len(t) > 1 && t[1] == "least":
		isOver, skip = true, 2
	case under[w] == 1:
		isUnder, skip = true, 1
	case over[w] == 1:
		isOver, skip = true, 1
	default:
		// "<2h" glued to the value.
		if strings.HasPrefix(w, "<") || strings.HasPrefix(w, ">") {
			isUnder = w[0] == '<'
			isOver = !isUnder
			t = append([]string{w[1:]}, t[1:]...)
			skip = 0
		} else {
			return 0
		}
	}
	d, extra, ok := parseRuntimeValue(t[skip:])
	if !ok || d <= 0 || d > 24*time.Hour {
		return 0
	}
	used := skip + 1 + extra
	mins := int(d / time.Minute)
	if isUnder {
		p.filter.MaxRuntime = d
		p.runPart = "Under " + strconv.Itoa(mins) + " Minutes"
		// "100 mins or less"
		if len(t) > used+1 && t[used] == "or" && t[used+1] == "less" {
			used += 2
		}
	} else if isOver {
		p.filter.MinRuntime = d
		p.runPart = "Over " + strconv.Itoa(mins) + " Minutes"
	}
	p.signals++
	return used
}

// consumeRating handles "highly rated", "acclaimed", "top rated", "best".
func (p *vibeParser) consumeRating(t []string) int {
	set := func(min float64, part string) {
		if min > p.filter.MinRating {
			p.filter.MinRating = min
		}
		p.sort, p.desc = core.SortRating, true
		p.ratingPart = part
		p.signals++
	}
	join2 := ""
	if len(t) > 1 {
		join2 = t[0] + " " + t[1]
	}
	switch join2 {
	case "highly rated", "well rated", "top rated", "best rated",
		"highest rated", "critically acclaimed":
		set(7.5, "Highly Rated")
		return 2
	}
	switch t[0] {
	case "acclaimed":
		set(7.5, "Acclaimed")
		return 1
	case "best", "top", "greatest":
		set(8, "Best")
		return 1
	case "rated":
		// Only as part of phrases above; bare "rated" is filler next
		// to a content rating we do not parse.
		return 0
	}
	// "rated 8+", "8+ rated", "above 7.5" are intentionally unsupported.
	return 0
}

// title builds the human collection title from the consumed parts.
func (p *vibeParser) title() string {
	var parts []string
	add := func(s string) {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if p.person != "" {
		return titleCaseName(p.person) + " " + mediaNoun(p.media, "Films")
	}
	if p.studioDisp != "" && len(p.filter.Genres) == 0 {
		return p.studioDisp + " " + mediaNoun(p.media, "Movies")
	}
	add(p.ratingPart)
	add(p.moodPart)
	if p.recent && p.decadePart == "" && p.yearPart == "" {
		add("Latest")
	}
	add(p.decadePart)
	add(p.yearPart)
	add(p.langPart)
	if p.studioDisp != "" {
		add(p.studioDisp)
	}
	switch {
	case p.genreDisp != "":
		add(p.genreDisp)
	case p.genreAdj != "":
		add(p.genreAdj + " " + mediaNoun(p.media, "Movies"))
	default:
		add(mediaNoun(p.media, "Movies"))
	}
	add(p.runPart)
	return strings.Join(parts, " ")
}

func mediaNoun(t core.MediaType, def string) string {
	if t == core.TypeShow {
		return "Shows"
	}
	if t == core.TypeMovie {
		return "Movies"
	}
	return def
}

// titleCaseName capitalizes a lower-cased name, keeping small words low.
func titleCaseName(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if i > 0 {
			switch w {
			case "the", "a", "an", "of", "and":
				continue
			}
		}
		r := []rune(w)
		if len(r) > 0 {
			r[0] = unicode.ToUpper(r[0])
			words[i] = string(r)
		}
	}
	return strings.Join(words, " ")
}

var similarRe = regexp.MustCompile(`(?i)^\s*(?:(movies?|films?|shows?|series|tv(?:\s+shows?)?)\s+)?(?:like|similar\s+to|in\s+the\s+vein\s+of|more\s+like)\s+(.+?)\s*$`)

// parseSimilar handles "shows like the office" and "like dark".
func parseSimilar(q string) (core.VibeQuery, bool) {
	m := similarRe.FindStringSubmatch(q)
	if m == nil {
		// "more office-like shows" etc. are left to plain search.
		return core.VibeQuery{}, false
	}
	seed := strings.Trim(strings.TrimSpace(m[2]), `"'`)
	if seed == "" || len(strings.Fields(seed)) > 8 {
		return core.VibeQuery{}, false
	}
	// A seed that is itself only vibe words ("like horror") is not a seed.
	if _, isGenre := vibeGenres[strings.ToLower(seed)]; isGenre {
		return core.VibeQuery{}, false
	}
	v := core.VibeQuery{Query: q, SimilarSeed: seed, Sort: core.SortPopularity, Desc: true}
	mt := strings.ToLower(m[1])
	switch {
	case strings.HasPrefix(mt, "movie"), strings.HasPrefix(mt, "film"):
		v.Filter.Type = core.TypeMovie
	case mt != "":
		v.Filter.Type = core.TypeShow
	}
	v.Title = mediaNoun(v.Filter.Type, "Titles") + " Like " + titleCaseName(strings.ToLower(seed))
	if v.Filter.Type == "" {
		v.Title = "More Like " + titleCaseName(strings.ToLower(seed))
	}
	return v, true
}

var byPersonRe = regexp.MustCompile(`(?i)^\s*(?:(movies?|films?|shows?|series)\s+)?(?:by|directed\s+by|starring|with)\s+(.+?)\s*$`)

// parseByPerson handles "films by christopher nolan" shapes. A leading
// media word or a directed/starring verb is required, so plain titles
// containing "by" are not swallowed.
func parseByPerson(q string) (core.VibeQuery, bool) {
	low := strings.ToLower(q)
	if !strings.Contains(low, "directed by") && !strings.Contains(low, "starring") {
		// Bare "by"/"with" need the "<media> by X" shape.
		if byPersonRe.FindStringSubmatch(q) == nil {
			return core.VibeQuery{}, false
		}
		m := byPersonRe.FindStringSubmatch(q)
		if m[1] == "" {
			return core.VibeQuery{}, false
		}
	}
	m := byPersonRe.FindStringSubmatch(q)
	if m == nil {
		return core.VibeQuery{}, false
	}
	name := strings.Trim(strings.TrimSpace(m[2]), `"'`)
	fields := strings.Fields(name)
	if len(fields) < 2 || len(fields) > 4 {
		return core.VibeQuery{}, false
	}
	v := core.VibeQuery{Query: q, PersonName: strings.ToLower(name), Sort: core.SortPopularity, Desc: true}
	switch strings.ToLower(m[1]) {
	case "show", "shows", "series":
		v.Filter.Type = core.TypeShow
	case "movie", "movies", "film", "films":
		v.Filter.Type = core.TypeMovie
	}
	v.Title = titleCaseName(v.PersonName) + " " + mediaNoun(v.Filter.Type, "Films")
	return v, true
}

// parseTopicVibe leaves free-form topics to bounded catalogue keyword and
// overview matching. Keep ParseVibe conservative for its standalone callers.
func parseTopicVibe(query string) (core.VibeQuery, bool) {
	q := strings.TrimSpace(query)
	if q == "" || len(q) > 300 {
		return core.VibeQuery{}, false
	}
	topic := strings.ToLower(q)
	var typ core.MediaType
	for _, prefix := range []string{"movies about ", "films about ", "shows about ", "series about ", "about "} {
		if strings.HasPrefix(topic, prefix) {
			if strings.HasPrefix(prefix, "movies") || strings.HasPrefix(prefix, "films") {
				typ = core.TypeMovie
			}
			if strings.HasPrefix(prefix, "shows") || strings.HasPrefix(prefix, "series") {
				typ = core.TypeShow
			}
			topic = strings.TrimSpace(strings.TrimPrefix(topic, prefix))
			break
		}
	}
	words := strings.Fields(topic)
	if len(words) == 0 || len(words) > 12 {
		return core.VibeQuery{}, false
	}
	for _, word := range words {
		for _, r := range word {
			if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '-' {
				return core.VibeQuery{}, false
			}
		}
	}
	return core.VibeQuery{Query: q, Title: q, Filter: core.Filter{Type: typ, Topic: strings.Join(words, " ")}, Sort: core.SortPopularity, Desc: true}, true
}
