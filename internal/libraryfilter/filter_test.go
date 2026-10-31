package libraryfilter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestInvalidFiltersCannotHideWholeLibrary(t *testing.T) {
	for _, g := range []core.LibraryExclusion{
		{Name: "Empty", MediaType: "mixed", Match: "all"},
		{Name: "SQL", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{{Field: "country; DROP TABLE", Op: "in", Value: json.RawMessage(`["IN"]`)}}},
		{Name: "Bad range", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{{Field: "year", Op: "between", Value: json.RawMessage(`[2026,1900]`)}}},
	} {
		if _, err := Compile([]core.LibraryExclusion{g}); err == nil {
			t.Fatal("invalid rule accepted", g)
		}
	}
}

func movieRule(match string, rules ...core.CollectionRule) []core.LibraryExclusion {
	return []core.LibraryExclusion{{Name: "Group", MediaType: "movie", Match: match, Rules: rules}}
}

func textRule(field, op, raw string) core.CollectionRule {
	return core.CollectionRule{Field: field, Op: op, Value: json.RawMessage(raw)}
}

func mustCompile(t *testing.T, specs []core.LibraryExclusion) *Filter {
	t.Helper()
	f, err := Compile(specs)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Content rating "none of" is an allowlist of age levels: listed levels stay
// visible, and a missing certification is excluded unless "Not rated" is
// listed. Unknown country, language and year still do not match.
func TestContentRatingNotInFailsClosed(t *testing.T) {
	allow := mustCompile(t, movieRule("any", textRule("contentRating", "notIn", `["G","PG","PG-13","PG13","M"]`)))
	movie := func(rating string) *core.Title {
		return &core.Title{Type: core.TypeMovie, Title: rating, ContentRating: rating}
	}
	for _, rating := range []string{"G", "PG", "PG-13", "pg-13", "PG13", "M", "TV-G", "TV-PG", "TV-14", "gb/12A", "TV-Y7"} {
		if allow.Hidden(movie(rating)) {
			t.Fatalf("allowed rating %q hidden", rating)
		}
	}
	for _, rating := range []string{"", "R", "NR", "NC-17", "TV-MA", "de/16"} {
		if !allow.Hidden(movie(rating)) {
			t.Fatalf("rating %q still visible", rating)
		}
	}
	if allow.Hidden(&core.Title{Type: core.TypeShow}) {
		t.Fatal("movie rule hid a show")
	}
	sql, args := allow.SQLHidden()
	if !strings.Contains(sql, "NOT (t.content_rating_rank IN (?,?,?))") {
		t.Fatalf("negative rating SQL = %s", sql)
	}
	if len(args) != 4 || args[0] != "movie" || args[1] != 1 || args[2] != 2 || args[3] != 3 {
		t.Fatalf("args = %#v", args)
	}

	block := mustCompile(t, movieRule("any", textRule("contentRating", "in", `["R"]`)))
	if block.Hidden(movie("")) || block.Hidden(movie("PG")) || !block.Hidden(movie("R")) {
		t.Fatal("positive content-rating rule changed")
	}
	notR := mustCompile(t, movieRule("any", textRule("contentRating", "isNot", `"R"`)))
	if !notR.Hidden(movie("")) || notR.Hidden(movie("R")) || !notR.Hidden(movie("PG")) {
		t.Fatal("isNot did not fail closed")
	}

	nestedAll := mustCompile(t, movieRule("all",
		textRule("contentRating", "notIn", `["G","PG","PG-13","PG13","M"]`),
		textRule("genre", "in", `["Horror"]`),
	))
	if !nestedAll.Hidden(&core.Title{Type: core.TypeMovie, Genres: []string{"Horror"}}) ||
		nestedAll.Hidden(&core.Title{Type: core.TypeMovie, Genres: []string{"Comedy"}}) ||
		nestedAll.Hidden(&core.Title{Type: core.TypeMovie, ContentRating: "PG", Genres: []string{"Horror"}}) {
		t.Fatal("nested all")
	}
	nestedAny := mustCompile(t, movieRule("any",
		textRule("contentRating", "notIn", `["G","PG"]`),
		textRule("country", "in", `["IN"]`),
	))
	if nestedAny.Hidden(&core.Title{Type: core.TypeMovie, ContentRating: "PG", Countries: []string{"US"}}) ||
		!nestedAny.Hidden(&core.Title{Type: core.TypeMovie, Countries: []string{"US"}}) ||
		!nestedAny.Hidden(&core.Title{Type: core.TypeMovie, ContentRating: "PG", Countries: []string{"IN"}}) {
		t.Fatal("nested any")
	}

	country := mustCompile(t, []core.LibraryExclusion{{
		Name: "Country", MediaType: "mixed", Match: "all",
		Rules: []core.CollectionRule{textRule("country", "notIn", `["IN"]`)},
	}})
	if country.Hidden(&core.Title{Type: core.TypeMovie}) ||
		!country.Hidden(&core.Title{Type: core.TypeMovie, Countries: []string{"France"}}) {
		t.Fatal("unknown country rule changed")
	}
	year := mustCompile(t, []core.LibraryExclusion{{
		Name: "Year", MediaType: "mixed", Match: "all",
		Rules: []core.CollectionRule{textRule("year", "gte", `2000`)},
	}})
	if year.Hidden(&core.Title{Type: core.TypeMovie}) || !year.Hidden(&core.Title{Type: core.TypeMovie, Year: 2020}) {
		t.Fatal("unknown year rule changed")
	}
	lang := mustCompile(t, []core.LibraryExclusion{{
		Name: "Language", MediaType: "mixed", Match: "all",
		Rules: []core.CollectionRule{textRule("language", "notIn", `["en"]`)},
	}})
	if lang.Hidden(&core.Title{Type: core.TypeMovie}) ||
		lang.Hidden(&core.Title{Type: core.TypeMovie, Language: "en"}) ||
		!lang.Hidden(&core.Title{Type: core.TypeMovie, Language: "hi"}) {
		t.Fatal("unknown language rule changed")
	}
	countrySQL, _ := country.SQLHidden()
	if !strings.Contains(countrySQL, "t.key IN (SELECT key FROM catalog_countries) AND NOT (") {
		t.Fatalf("country SQL dropped the known-value guard: %s", countrySQL)
	}
	yearSQL, _ := year.SQLHidden()
	if !strings.Contains(yearSQL, "t.year>0") {
		t.Fatalf("year SQL dropped the known-value guard: %s", yearSQL)
	}
}

func TestContentRatingAllowlistMatchesEquivalentLevels(t *testing.T) {
	family := mustCompile(t, []core.LibraryExclusion{{Name: "Family", MediaType: "mixed", Match: "all",
		Rules: []core.CollectionRule{textRule("contentRating", "notIn", `["G","PG","TV-G","TV-PG"]`)}}})
	title := func(typ core.MediaType, rating string) *core.Title {
		return &core.Title{Type: typ, ContentRating: rating}
	}
	for _, tc := range []struct {
		t      *core.Title
		hidden bool
	}{
		{title(core.TypeMovie, "G"), false},     // Christmas in Evergreen (TMDB US type 6 "G")
		{title(core.TypeMovie, "PG"), false},    // Puppy Love
		{title(core.TypeMovie, "TV-G"), false},  // OMDb/IMDb certificate
		{title(core.TypeShow, "TV-G"), false},   // When Calls the Heart
		{title(core.TypeShow, "TV-PG"), false},  // Chesapeake Shores
		{title(core.TypeMovie, "ca/G"), false},  // Write Before Christmas (Canada only)
		{title(core.TypeMovie, "gb/PG"), false}, // UK PG
		{title(core.TypeShow, "TV-Y"), false},   // all ages
		{title(core.TypeMovie, "se/7"), false},  // Just in Time for Christmas: Sweden's 7 is PG's level
		{title(core.TypeMovie, "de/6"), false},  // Summer Villa: Germany's 6 likewise
		{title(core.TypeShow, "TV-Y7"), false},  // kids' 7+
		{title(core.TypeMovie, "PG-13"), true},
		{title(core.TypeMovie, "R"), true},
		{title(core.TypeMovie, ""), true},
		{title(core.TypeMovie, "NR"), true},
	} {
		if got := family.Hidden(tc.t); got != tc.hidden {
			t.Errorf("%s %q hidden=%v, want %v", tc.t.Type, tc.t.ContentRating, got, tc.hidden)
		}
	}
	withUnrated := mustCompile(t, []core.LibraryExclusion{{Name: "Family", MediaType: "mixed", Match: "all",
		Rules: []core.CollectionRule{textRule("contentRating", "notIn", `["G","PG","NR"]`)}}})
	if withUnrated.Hidden(title(core.TypeMovie, "")) || withUnrated.Hidden(title(core.TypeMovie, "Not Rated")) || !withUnrated.Hidden(title(core.TypeMovie, "R")) {
		t.Fatal(`"Not rated" did not keep unrated titles`)
	}
	sql, args := withUnrated.SQLHidden()
	if sql != "(((NOT (t.content_rating_rank IN (?,?,?)))))" || len(args) != 3 || args[2] != 0 {
		t.Fatalf("SQL %s %#v", sql, args)
	}
}

// "Hide unrated thrillers": unrated titles combined with another condition,
// so family-safe unrated titles stay and unrated adult ones go.
func TestNotRatedCombinesWithOtherConditions(t *testing.T) {
	f := mustCompile(t, []core.LibraryExclusion{{Name: "Unrated thrillers", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{
		textRule("contentRating", "in", `["NR"]`),
		textRule("genre", "in", `["Thriller","Horror"]`),
	}}})
	if !f.Hidden(&core.Title{Type: core.TypeMovie, Genres: []string{"Thriller"}}) {
		t.Fatal("unrated thriller visible")
	}
	if f.Hidden(&core.Title{Type: core.TypeMovie, Genres: []string{"TV Movie", "Romance"}}) {
		t.Fatal("unrated TV movie hidden")
	}
	if f.Hidden(&core.Title{Type: core.TypeMovie, ContentRating: "R", Genres: []string{"Thriller"}}) {
		t.Fatal("rated thriller hidden by an unrated rule")
	}
}

func TestContentRatingBounds(t *testing.T) {
	above := mustCompile(t, movieRule("all", textRule("contentRating", "gte", `"TV-14"`)))
	for rating, hidden := range map[string]bool{"PG-13": true, "R": true, "TV-MA": true, "gb/15": true, "PG": false, "TV-PG": false, "": false, "NR": false} {
		if got := above.Hidden(&core.Title{Type: core.TypeMovie, ContentRating: rating}); got != hidden {
			t.Errorf("at least TV-14: %q hidden=%v", rating, got)
		}
	}
	sql, args := above.SQLHidden()
	if !strings.Contains(sql, "t.content_rating_rank>0 AND t.content_rating_rank>=?") || args[1] != 3 {
		t.Fatalf("SQL %s %#v", sql, args)
	}
	for _, bad := range []string{`"NR"`, `["PG","R"]`, `"bogus"`} {
		if _, err := Compile(movieRule("all", textRule("contentRating", "lte", bad))); err == nil {
			t.Errorf("lte %s accepted", bad)
		}
	}
	junk := mustCompile(t, movieRule("all", textRule("contentRating", "in", `["Yes"]`)))
	if junk.Hidden(&core.Title{Type: core.TypeMovie, ContentRating: "Yes"}) || junk.Hidden(&core.Title{Type: core.TypeMovie}) {
		t.Fatal("an unknown label matched")
	}
}

func TestLegacyHideRulesMatchEitherGroup(t *testing.T) {
	f := mustCompile(t, []core.LibraryExclusion{
		{Name: "India", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{textRule("country", "in", `["IN"]`)}},
		{Name: "Low", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{textRule("rating", "lte", `4`)}},
	})
	if !f.Hidden(&core.Title{Type: core.TypeMovie, Countries: []string{"India"}, Rating: 9}) {
		t.Fatal("india")
	}
	if !f.Hidden(&core.Title{Type: core.TypeMovie, Countries: []string{"US"}, Rating: 3}) {
		t.Fatal("low")
	}
	if f.Hidden(&core.Title{Type: core.TypeMovie, Countries: []string{"US"}, Rating: 8}) {
		t.Fatal("kept")
	}
	if f.Hidden(&core.Title{Type: core.TypeShow, Rating: 3}) {
		t.Fatal("movie rule hid a show")
	}
	if f.Fallback() != "show" || f.Passive() {
		t.Fatal("legacy default")
	}
}

// Saved exclusion lists have no action and no default. They stay hide rules
// with a show default, so a title is hidden exactly when any group matches.
func TestExistingConfigsKeepIdenticalVisibility(t *testing.T) {
	configs := [][]core.LibraryExclusion{
		nil,
		{{Name: "India", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{textRule("country", "in", `["IN"]`)}}},
		{
			{Name: "India", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{textRule("country", "in", `["IN"]`)}},
			{Name: "Low", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{textRule("rating", "lte", `4`)}},
		},
		{{Name: "Family", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{textRule("contentRating", "notIn", `["G","PG","TV-G","TV-PG"]`)}}},
		{{Name: "Either", MediaType: "show", Match: "any", Rules: []core.CollectionRule{textRule("genre", "in", `["Horror"]`), textRule("language", "is", `"hi"`)}}},
		{
			{Name: "Movies", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{textRule("year", "gte", `2020`)}},
			{Name: "Shows", MediaType: "show", Match: "all", Rules: []core.CollectionRule{textRule("country", "notIn", `["US"]`)}},
		},
	}
	titles := []*core.Title{
		{Type: core.TypeMovie, Title: "blank"},
		{Type: core.TypeMovie, Countries: []string{"India"}, Rating: 9, ContentRating: "PG", Year: 2019},
		{Type: core.TypeMovie, Countries: []string{"US"}, Rating: 3, ContentRating: "R", Year: 2024, Language: "en"},
		{Type: core.TypeMovie, Countries: []string{"France"}, Rating: 8, ContentRating: "TV-PG", Genres: []string{"Horror"}, Year: 2021},
		{Type: core.TypeShow, Countries: []string{"IN"}, Rating: 2, ContentRating: "TV-MA", Genres: []string{"Drama"}, Language: "hi", Year: 2018},
		{Type: core.TypeShow, ContentRating: "G", Genres: []string{"Horror"}, Language: "en"},
		{Type: core.TypeEpisode, ContentRating: "R", Countries: []string{"IN"}},
	}
	for i, specs := range configs {
		whole := mustCompile(t, specs)
		sql, _ := whole.SQLHidden()
		if strings.Contains(sql, "CASE") {
			t.Fatalf("config %d compiled a first-match CASE: %s", i, sql)
		}
		for _, title := range titles {
			want := false
			for _, spec := range specs {
				if mustCompile(t, []core.LibraryExclusion{spec}).Hidden(title) {
					want = true
				}
			}
			if whole.Hidden(title) != want {
				t.Fatalf("config %d title %q hidden=%v want %v", i, title.Title, whole.Hidden(title), want)
			}
		}
	}
}

func TestFirstMatchShowBeatsLaterHide(t *testing.T) {
	f, err := CompilePolicy([]core.LibraryExclusion{
		{Name: "TV movies", Action: "show", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{textRule("genre", "is", `"TV Movie"`)}},
		{Name: "Above PG", Action: "hide", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{textRule("contentRating", "gte", `"PG-13"`)}},
	}, "show")
	if err != nil {
		t.Fatal(err)
	}
	tv := &core.Title{Type: core.TypeMovie, Genres: []string{"TV Movie"}, ContentRating: "R"}
	action := &core.Title{Type: core.TypeMovie, Genres: []string{"Action"}, ContentRating: "R"}
	family := &core.Title{Type: core.TypeMovie, Genres: []string{"Action"}, ContentRating: "PG"}
	if f.Hidden(tv) || !f.Hidden(action) || f.Hidden(family) {
		t.Fatalf("tv %v action %v family %v", f.Hidden(tv), f.Hidden(action), f.Hidden(family))
	}
	sql, _ := f.SQLHidden()
	if !strings.Contains(sql, "CASE WHEN") || !strings.Contains(sql, "ELSE 0") {
		t.Fatalf("SQL %s", sql)
	}
	hideRest, err := CompilePolicy(nil, "hide")
	sqlHide, _ := hideRest.SQLHidden()
	if err != nil || !hideRest.Hidden(&core.Title{Type: core.TypeMovie, ContentRating: "G"}) || sqlHide != "1=1" {
		t.Fatal("default hide", err, sqlHide)
	}
}

func TestUnknownAndStudioKeyword(t *testing.T) {
	year, err := Compile([]core.LibraryExclusion{{Name: "No year", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{textRule("year", "unknown", `null`)}}})
	if err != nil || !year.Hidden(&core.Title{Type: core.TypeMovie}) || year.Hidden(&core.Title{Type: core.TypeMovie, Year: 1999}) {
		t.Fatal("unknown year", err)
	}
	studio := mustCompile(t, []core.LibraryExclusion{{Name: "Studio", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{textRule("studio", "is", `"Amblin"`)}}})
	if !studio.Hidden(&core.Title{Type: core.TypeMovie, Studios: []string{"Amblin"}}) || studio.Hidden(&core.Title{Type: core.TypeMovie}) {
		t.Fatal("studio")
	}
	kw := mustCompile(t, []core.LibraryExclusion{{Name: "Word", MediaType: "show", Match: "all", Rules: []core.CollectionRule{textRule("keyword", "is", `{"name":"heist"}`)}}})
	if !kw.Hidden(&core.Title{Type: core.TypeShow, Keywords: []string{"Heist"}}) {
		t.Fatal("keyword")
	}
	sql, _ := studio.SQLHidden()
	if !strings.Contains(sql, "catalog_studios") {
		t.Fatal(sql)
	}
}
