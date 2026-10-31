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

// Content rating "none of" is an allowlist: listed levels stay visible, and
// a missing certification is excluded unless "Not rated" is listed.
// Unknown country, language and year still do not match.
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
	if !strings.Contains(countrySQL, "EXISTS (SELECT 1 FROM catalog_countries") {
		t.Fatalf("country SQL dropped the known-value guard: %s", countrySQL)
	}
	yearSQL, _ := year.SQLHidden()
	if !strings.Contains(yearSQL, "t.year>0") {
		t.Fatalf("year SQL dropped the known-value guard: %s", yearSQL)
	}
}

// The reported case: an allowlist of G, PG, TV-G and TV-PG. Hallmark TV
// movies TMDB certifies "G" or "PG" (on their TV premiere release), TV
// series rated TV-G, foreign-only ratings kept region-qualified and the
// equivalent kids' ratings all stay; mature and unrated titles are hidden
// until "Not rated" is listed.
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
	// A label no system knows names no level, so it matches nothing.
	junk := mustCompile(t, movieRule("all", textRule("contentRating", "in", `["Yes"]`)))
	if junk.Hidden(&core.Title{Type: core.TypeMovie, ContentRating: "Yes"}) || junk.Hidden(&core.Title{Type: core.TypeMovie}) {
		t.Fatal("an unknown label matched")
	}
}
