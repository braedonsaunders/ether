package collections

import (
	"encoding/json"
	"net/url"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func cr(field, op string, v any) core.CollectionRule {
	b, _ := json.Marshal(v)
	return core.CollectionRule{Field: field, Op: op, Value: b}
}

func TestRuleEvaluationTable(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	e := &entry{
		key: core.MovieKey(1), typ: core.TypeMovie, title: "Heist", year: 1985,
		released: time.Date(1985, 7, 3, 0, 0, 0, 0, time.UTC).Unix(), added: now.Add(-5 * 24 * time.Hour).Unix(),
		runtime: 116, rating: 8.3, votes: 19000, pop: 75, lang: "en", crating: "PG-13", collID: 264,
		genresLC: []string{"adventure", "comedy", "science fiction"}, countriesLC: []string{"united states of america"},
		studiosLC: []string{"amblin entertainment"}, keywordsLC: []string{"time travel", "delorean"},
		cast: []int64{521}, directors: []int64{24}, writers: []int64{24, 25},
	}
	cases := []struct {
		name string
		r    core.CollectionRule
		want bool
	}{
		{"genre is", cr("genre", "is", "Comedy"), true},
		{"genre is all-of", cr("genre", "is", []string{"Comedy", "Adventure"}), true},
		{"genre is all-of miss", cr("genre", "is", []string{"Comedy", "Horror"}), false},
		{"genre in", cr("genre", "in", []string{"Horror", "Comedy"}), true},
		{"genre notIn", cr("genre", "notIn", []string{"Horror", "War"}), true},
		{"genre isNot", cr("genre", "isNot", "Comedy"), false},
		{"country by code", cr("country", "is", "US"), true},
		{"country by name", cr("country", "in", []string{"France", "United States of America"}), true},
		{"language", cr("language", "is", "en"), true},
		{"content rating is", cr("contentRating", "in", []string{"PG", "PG-13"}), true},
		{"content rating lte R", cr("contentRating", "lte", "R"), true},
		{"content rating lte PG", cr("contentRating", "lte", "PG"), false},
		{"content rating gte PG", cr("contentRating", "gte", "PG"), true},
		{"keyword name", cr("keyword", "is", "time travel"), true},
		{"keyword object", cr("keyword", "in", []map[string]any{{"id": 9, "name": "DeLorean"}}), true},
		{"keyword miss", cr("keyword", "is", "zombie"), false},
		{"keyword notIn", cr("keyword", "notIn", []string{"zombie"}), true},
		{"person any role", cr("person", "is", 24), true},
		{"person cast only", cr("person", "is", map[string]any{"id": 24, "role": "cast"}), false},
		{"person director", cr("person", "is", map[string]any{"id": 24, "role": "director"}), true},
		{"person writer", cr("person", "in", []any{map[string]any{"id": 25, "role": "writer"}}), true},
		{"studio", cr("studio", "is", "Amblin Entertainment"), true},
		{"collection", cr("collection", "is", 264), true},
		{"year between", cr("year", "between", []int{1980, 1989}), true},
		{"year between reversed", cr("year", "between", []int{1989, 1980}), true},
		{"decade", cr("decade", "is", 1980), true},
		{"decade string", cr("decade", "is", "1980s"), true},
		{"decade in", cr("decade", "in", []int{1970, 1990}), false},
		{"rating gte", cr("rating", "gte", 8), true},
		{"rating lte", cr("rating", "lte", 8), false},
		{"votes", cr("votes", "gte", 20000), false},
		{"popularity", cr("popularity", "between", []float64{50, 80}), true},
		{"runtime", cr("runtime", "lte", 120), true},
		{"released between", cr("released", "between", []string{"1985-01-01", "1985-12-31"}), true},
		// Year/month-precision upper bounds include the whole period.
		{"released between years", cr("released", "between", []string{"1980", "1985"}), true},
		{"released lte year", cr("released", "lte", "1985"), true},
		{"released lte month", cr("released", "lte", "1985-07"), true},
		{"released lte earlier month", cr("released", "lte", "1985-06"), false},
		{"released lte day", cr("released", "lte", "1985-07-03"), true},
		{"released gte year", cr("released", "gte", "1986"), false},
		{"released gte relative", cr("released", "gte", "-2y"), false},
		{"released within", cr("released", "within", "45y"), true},
		{"released within days", cr("released", "within", 30), false},
		{"added within", cr("addedAt", "within", "7d"), true},
		{"added within short", cr("addedAt", "within", "2d"), false},
	}
	ec := &evalCtx{now: now}
	for _, c := range cases {
		r, err := compileRule(c.r)
		if err != nil {
			t.Fatalf("%s: compile: %v", c.name, err)
		}
		if got := r.match(e, ec); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	// Unknown data fails positive conditions and passes negative ones.
	bare := &entry{key: core.MovieKey(2), typ: core.TypeMovie}
	for _, c := range []struct {
		r    core.CollectionRule
		want bool
	}{
		{cr("runtime", "lte", 90), false},
		{cr("year", "isNot", 1999), true},
		{cr("keyword", "isNot", "zombie"), true},
		{cr("released", "within", "30d"), false},
	} {
		r, _ := compileRule(c.r)
		if got := r.match(bare, ec); got != c.want {
			t.Errorf("unknown data %s %s: got %v", c.r.Field, c.r.Op, got)
		}
	}
	// Personal rules.
	w, _ := compileRule(cr("watched", "is", true))
	ip, _ := compileRule(cr("inProgress", "isNot", true))
	pec := &evalCtx{now: now, status: map[core.RatingKey]core.WatchStatus{e.key: {ViewCount: 2}}}
	if !w.match(e, pec) || !ip.match(e, pec) {
		t.Fatal("personal rules")
	}
}

func TestCompileRuleErrors(t *testing.T) {
	bad := []core.CollectionRule{
		cr("genre", "between", []string{"a", "b"}),
		cr("rating", "between", 7),
		cr("rating", "gte", "high"),
		cr("year", "gte", []int{1, 2}),
		cr("released", "within", "-"),
		cr("released", "between", []string{"2020-01-01"}),
		cr("watched", "is", "maybe"),
		cr("person", "is", map[string]any{"role": "director"}),
		cr("person", "is", map[string]any{"id": 1, "role": "grip"}),
		cr("genre", "is", []any{[]string{"nested"}}),
		cr("keyword", "is", -3),
		{Field: "genre", Op: "is"},
	}
	for _, r := range bad {
		if _, err := compileRule(r); err == nil {
			t.Errorf("%s %s %s: want error", r.Field, r.Op, r.Value)
		}
	}
}

func TestNormalizeDefaults(t *testing.T) {
	sp := core.CollectionSpec{Name: "  x  ", MediaType: "tv"}
	Normalize(&sp)
	if sp.Name != "x" || sp.MediaType != MediaShow || sp.Match != MatchAll || sp.Sort != "popularity" || !sp.SortDesc ||
		sp.Refresh != RefreshDaily || sp.Artwork.Mode != "auto" || !sp.Targets.Users.All || len(sp.Sources) != 1 || sp.Sources[0].Kind != "catalog" {
		t.Fatalf("defaults: %+v", sp)
	}
	if err := Validate(&sp); err != nil {
		t.Fatal(err)
	}
	// An explicit empty user list targets nobody.
	var u core.UserTargets
	if err := json.Unmarshal([]byte(`[]`), &u); err != nil || u.All || u.Includes(&core.User{ID: 1}) {
		t.Fatalf("empty users: %+v %v", u, err)
	}
	if err := json.Unmarshal([]byte(`"all"`), &u); err != nil || !u.All || !u.Includes(nil) {
		t.Fatalf("all users: %+v", u)
	}
	if err := json.Unmarshal([]byte(`[1,"2"]`), &u); err != nil || !u.Includes(&core.User{ID: 2}) || u.Includes(nil) {
		t.Fatalf("ids: %+v %v", u, err)
	}
	b, _ := json.Marshal(core.UserTargets{IDs: []int64{4}})
	if string(b) != "[4]" {
		t.Fatalf("marshal: %s", b)
	}
}

func TestRotationIsDeterministicAndCoversEveryMember(t *testing.T) {
	keys := make([]core.RatingKey, 10)
	for i := range keys {
		keys[i] = core.MovieKey(int64(i + 1))
	}
	day := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	a := rotate(keys, 4, "daily", "c1", day)
	b := rotate(keys, 4, "daily", "c1", day.Add(5*time.Hour))
	if !reflect.DeepEqual(a, b) || len(a) != 4 {
		t.Fatalf("same day differs: %v %v", a, b)
	}
	// Order follows the collection's order.
	if !sort.SliceIsSorted(a, func(i, j int) bool { return a[i] < a[j] }) {
		t.Fatalf("rotation reordered: %v", a)
	}
	// One cycle (3 windows of 4 over 10) shows every member.
	period := periodIndex("daily", day)
	cycleStart := period - period%3
	seen := map[core.RatingKey]bool{}
	for d := int64(0); d < 3; d++ {
		at := day.AddDate(0, 0, int(cycleStart+d-period))
		for _, k := range rotate(keys, 4, "daily", "c1", at) {
			seen[k] = true
		}
	}
	if len(seen) != 10 {
		t.Fatalf("a cycle must cover all members, saw %d", len(seen))
	}
	if c := rotate(keys, 4, "daily", "c2", day); reflect.DeepEqual(c, a) && reflect.DeepEqual(rotate(keys, 4, "daily", "c2", day.AddDate(0, 0, 1)), rotate(keys, 4, "daily", "c1", day.AddDate(0, 0, 1))) {
		t.Fatal("different collections should rotate independently")
	}
	if w1, w2 := rotate(keys, 4, "weekly", "c1", day), rotate(keys, 4, "weekly", "c1", day.AddDate(0, 0, 1)); time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).Weekday() != time.Monday && !reflect.DeepEqual(w1, w2) {
		t.Fatal("weekly rotation changed mid-week")
	}
	if got := rotate(keys[:3], 4, "daily", "c1", day); len(got) != 3 {
		t.Fatal("small collections are shown whole")
	}
}

func TestScheduleAllows(t *testing.T) {
	fri21 := time.Date(2026, 10, 16, 21, 30, 0, 0, time.UTC)
	h := func(n int) *int { return &n }
	cases := []struct {
		sc     *core.CollectionSchedule
		device string
		at     time.Time
		want   bool
	}{
		{nil, "tv", fri21, true},
		{&core.CollectionSchedule{Days: []int{5}}, "", fri21, true},
		{&core.CollectionSchedule{Days: []int{0, 6}}, "", fri21, false},
		{&core.CollectionSchedule{FromHour: h(17), ToHour: h(22)}, "", fri21, true},
		{&core.CollectionSchedule{FromHour: h(17), ToHour: h(21)}, "", fri21, false},
		{&core.CollectionSchedule{FromHour: h(22), ToHour: h(2)}, "", fri21, false},
		{&core.CollectionSchedule{FromHour: h(22), ToHour: h(2)}, "", fri21.Add(3 * time.Hour), true},
		{&core.CollectionSchedule{FromHour: h(9), ToHour: h(9)}, "", fri21, true},
		{&core.CollectionSchedule{Devices: []string{"tv"}}, "mobile", fri21, false},
		{&core.CollectionSchedule{Devices: []string{"tv"}}, "", fri21, true},
	}
	for i, c := range cases {
		if got := scheduleAllows(c.sc, c.device, c.at); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestMergeDiscoverParams(t *testing.T) {
	q := url.Values{"with_keywords": {"1"}, "vote_count.gte": {"100"}, "primary_release_date.lte": {"2000-12-31"}}
	if !mergeParams(q, url.Values{"with_keywords": {"2"}, "vote_count.gte": {"50"}, "primary_release_date.lte": {"1999-01-01"}}) {
		t.Fatal("compatible merge refused")
	}
	if q.Get("with_keywords") != "1,2" || q.Get("vote_count.gte") != "100" || q.Get("primary_release_date.lte") != "1999-01-01" {
		t.Fatalf("merged: %v", q)
	}
	if mergeParams(q, url.Values{"with_keywords": {"3|4"}}) {
		t.Fatal("an OR list cannot be ANDed into an existing list")
	}
	if mergeParams(url.Values{"watch_region": {"US"}}, url.Values{"watch_region": {"GB"}}) {
		t.Fatal("conflicting scalars merged")
	}
}

func TestParseListURLs(t *testing.T) {
	good := map[string]func(string) (string, error){
		"https://trakt.tv/users/alice/lists/my-list":           parseTraktURL,
		"trakt.tv/users/alice/lists/123?sort=rank":             parseTraktURL,
		"https://app.trakt.tv/lists/987":                       parseTraktURL,
		"https://letterboxd.com/someone/list/best-of/":         parseLetterboxdURL,
		"https://www.letterboxd.com/someone/list/best-of/by/x": parseLetterboxdURL,
		"https://mdblist.com/lists/bob/top-picks":              parseMDBListURL,
		"https://mdblist.com/lists/bob/top/json":               parseMDBListURL,
	}
	for u, fn := range good {
		if _, err := fn(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if p, _ := parseTraktURL("https://trakt.tv/users/alice/lists/my-list"); p != "/users/alice/lists/my-list/items" {
		t.Fatalf("trakt path: %s", p)
	}
	bad := map[string]func(string) (string, error){
		"https://evil.example/users/a/lists/b": parseTraktURL,
		"ftp://trakt.tv/users/a/lists/b":       parseTraktURL,
		"https://letterboxd.com/film/heat/":    parseLetterboxdURL,
		"":                                     parseMDBListURL,
	}
	for u, fn := range bad {
		if _, err := fn(u); err == nil {
			t.Errorf("%q: want an error", u)
		}
	}
}

func TestParseLetterboxdMarkup(t *testing.T) {
	page := []byte(`<li class="poster-container"><div class="film-poster" data-film-slug="heat-1995" data-film-release-year="1995"><img alt="Heat &amp; Dust" /></div></li>
<div class="react-component" data-item-name="Ran (1985)" data-item-slug="ran"></div>
<div class="react-component" data-item-name="Ran (1985)" data-item-slug="ran"></div>
<a class="next" href="/u/list/l/page/3/">Next</a>`)
	refs, next := parseLetterboxdList(page)
	if next != "/u/list/l/page/3/" || len(refs) != 2 {
		t.Fatalf("refs %+v next %q", refs, next)
	}
	if refs[0].Title != "Heat & Dust" || refs[0].Year != 1995 || refs[0].Slug != "heat-1995" {
		t.Fatalf("classic: %+v", refs[0])
	}
	if refs[1].Title != "Ran" || refs[1].Year != 1985 {
		t.Fatalf("lazy poster: %+v", refs[1])
	}
}

func TestExtractObjectToleratesModelNoise(t *testing.T) {
	cases := map[string]string{
		"<think>hmm {not this}</think>{\"a\":1}":    `{"a":1}`,
		"Here:\n```json\n{\"a\": [1, 2,]}\n```\nok": `{"a": [1, 2]}`,
		"prefix {\"s\": \"a } b\"} suffix":          `{"s": "a } b"}`,
		"no json here":                              "",
	}
	for in, want := range cases {
		if got := extractObject(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

// Content ratings match by age level, so film and TV labels (and other
// boards' labels) are one choice; "NR" picks titles without a rating.
func TestContentRatingRulesMatchByLevel(t *testing.T) {
	ec := &evalCtx{now: time.Now()}
	for _, c := range []struct {
		crating string
		r       core.CollectionRule
		want    bool
	}{
		{"TV-PG", cr("contentRating", "in", []string{"PG"}), true},
		{"PG", cr("contentRating", "is", "TV-PG"), true},
		{"gb/PG", cr("contentRating", "in", []string{"TV-PG"}), true},
		{"TV-14", cr("contentRating", "in", []string{"PG-13"}), true},
		{"TV-14", cr("contentRating", "lte", "PG-13"), true},
		{"TV-MA", cr("contentRating", "gte", "R"), true},
		{"TV-MA", cr("contentRating", "notIn", []string{"G", "PG"}), true},
		{"", cr("contentRating", "in", []string{"NR"}), true},
		{"NR", cr("contentRating", "in", []string{"Not rated"}), true},
		{"", cr("contentRating", "notIn", []string{"G", "PG"}), true},
		{"", cr("contentRating", "lte", "R"), false},
		{"PG", cr("contentRating", "in", []string{"Yes"}), false},
	} {
		r, err := compileRule(c.r)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.match(&entry{key: core.MovieKey(1), typ: core.TypeMovie, crating: c.crating}, ec); got != c.want {
			t.Errorf("%q %s %s: got %v", c.crating, c.r.Op, c.r.Value, got)
		}
	}
	s := &Service{}
	for _, c := range []struct {
		r    core.CollectionRule
		want url.Values // nil: not pushed down
	}{
		{cr("contentRating", "in", []string{"TV-G", "TV-PG"}), url.Values{"certification": {"G|PG"}, "certification_country": {"US"}}},
		{cr("contentRating", "lte", "TV-14"), url.Values{"certification.lte": {"PG-13"}, "certification_country": {"US"}}},
		{cr("contentRating", "in", []string{"PG", "NR"}), nil},
		{cr("contentRating", "in", []string{"TV-Y7"}), nil},
	} {
		r, err := compileRule(c.r)
		if err != nil {
			t.Fatal(err)
		}
		q := url.Values{}
		ok := s.translate(t.Context(), r, "movie", q)
		if c.want == nil {
			if ok || len(q) != 0 {
				t.Errorf("%s pushed down: %v", c.r.Value, q)
			}
			continue
		}
		if !ok || !reflect.DeepEqual(q, c.want) {
			t.Errorf("%s %s pushdown %v %v", c.r.Op, c.r.Value, ok, q)
		}
	}
}
