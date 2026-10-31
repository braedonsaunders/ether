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

// Content rating "none of" is an allowlist: known listed ratings stay visible,
// and a missing certification is excluded. PG-13 and PG13 stay distinct strings.
// Unknown country, language and year still do not match.
func TestContentRatingNotInFailsClosed(t *testing.T) {
	allow := mustCompile(t, movieRule("any", textRule("contentRating", "notIn", `["G","PG","PG-13","PG13","M"]`)))
	movie := func(rating string) *core.Title {
		return &core.Title{Type: core.TypeMovie, Title: rating, ContentRating: rating}
	}
	for _, rating := range []string{"G", "PG", "PG-13", "pg-13", "PG13", "M"} {
		if allow.Hidden(movie(rating)) {
			t.Fatalf("allowed rating %q hidden", rating)
		}
	}
	for _, rating := range []string{"", "R", "NR", "NC-17", "TV-MA"} {
		if !allow.Hidden(movie(rating)) {
			t.Fatalf("rating %q still visible", rating)
		}
	}
	if allow.Hidden(&core.Title{Type: core.TypeShow}) {
		t.Fatal("movie rule hid a show")
	}
	sql, args := allow.SQLHidden()
	if strings.Contains(sql, "content_rating<>''") || !strings.Contains(sql, "NOT (LOWER(t.content_rating) IN (") {
		t.Fatalf("negative rating SQL = %s", sql)
	}
	if len(args) != 6 || args[0] != "movie" || args[1] != "g" || args[3] != "pg-13" || args[4] != "pg13" {
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
