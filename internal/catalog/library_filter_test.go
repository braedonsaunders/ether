package catalog

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLibraryFilterPreviewBrowseAndPolicyAgree(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	titles := []core.Title{
		{Type: core.TypeMovie, Countries: []string{"India"}, Language: "en", Rating: 7, Genres: []string{"Drama"}},
		{Type: core.TypeShow, Countries: []string{"IN", "US"}, Language: "en", Rating: 8},
		{Type: core.TypeMovie, Countries: []string{"United States of America"}, Language: "hi", Rating: 4},
		{Type: core.TypeMovie, Language: "hi", Rating: 5},
		{Type: core.TypeMovie, Countries: []string{"CA"}, Language: "en", Rating: 8},
	}
	for i := range titles {
		tt := &titles[i]
		tt.Key = core.MovieKey(int64(i + 1))
		if tt.Type == core.TypeShow {
			tt.Key = core.ShowKey(int64(i + 1))
		}
		tt.Title = "Sample"
		tt.Year = 2000
		tt.Released = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		if err = c.UpsertTitle(ctx, *tt, true); err != nil {
			t.Fatal(err)
		}
	}
	rule := func(f, op, v string) core.CollectionRule {
		return core.CollectionRule{Field: f, Op: op, Value: json.RawMessage(v)}
	}
	for _, specs := range [][]core.LibraryExclusion{
		nil,
		{{Name: "No matches", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("year", "gte", `2100`)}}},
		{{Name: "Country", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("country", "in", `["IN"]`)}}},
		{{Name: "Movie country + rating", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{rule("country", "in", `["IN"]`), rule("rating", "gte", `6`)}}},
		{{Name: "Countries", MediaType: "movie", Match: "any", Rules: []core.CollectionRule{rule("country", "is", "\"US\""), rule("country", "is", "\"CA\"")}}, {Name: "TV", MediaType: "show", Match: "all", Rules: []core.CollectionRule{rule("rating", "between", `[7,9]`)}}},
		{{Name: "Known non-India", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("country", "notIn", `["IN"]`)}}},
	} {
		f, err := libraryfilter.Compile(specs)
		if err != nil {
			t.Fatal(err)
		}
		hidden := 0
		for i := range titles {
			if f.Hidden(&titles[i]) {
				hidden++
			}
		}
		c.UpdateConfig(config.Catalog{Exclusions: specs})
		p, err := c.Browse(ctx, core.BrowseQuery{})
		if err != nil || p.Total != len(titles)-hidden {
			t.Fatalf("SQL/evaluator differ: total=%d hidden=%d err=%v", p.Total, hidden, err)
		}
		raw, err := c.PreviewVisibility(ctx, specs)
		if err != nil {
			t.Fatal(err)
		}
		preview := raw.(VisibilityPreview)
		if preview.Items == nil {
			t.Fatal("empty preview must encode items as [], not null")
		}
		if preview.Hidden != hidden || preview.UnknownCountry != 1 {
			t.Fatalf("preview=%+v", preview)
		}
		for _, tt := range p.Items {
			if !c.Visible(&tt) {
				t.Fatal("hidden title served")
			}
		}
	}
	c.UpdateConfig(config.Catalog{})
	p, _ := c.Browse(ctx, core.BrowseQuery{})
	if p.Total != 5 {
		t.Fatal("clearing rules left cached exclusions", p.Total)
	}
}

func TestContentRatingNotInAgreesAndExcludesMissing(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := func(year int) time.Time {
		if year == 0 {
			return time.Time{}
		}
		return time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC)
	}
	type sample struct {
		id      int64
		show    bool
		title   string
		rating  string
		country string
		genre   string
		lang    string
		year    int
	}
	rows := []sample{
		{1, false, "Allowed PG", "PG", "US", "Drama", "en", 1974},
		{2, false, "Allowed PG-13", "PG-13", "US", "Drama", "en", 1999},
		{3, false, "Allowed PG13", "PG13", "US", "Drama", "en", 1998},
		{4, false, "Blocked R", "R", "US", "Horror", "en", 1974},
		{5, false, "Missing horror", "", "US", "Horror", "en", 1974},
		{6, false, "Missing comedy", "", "US", "Comedy", "en", 1974},
		{7, false, "India PG", "PG", "IN", "Drama", "hi", 2020},
		{8, false, "No country", "PG", "", "Drama", "en", 2001},
		{9, false, "No language", "PG", "US", "Drama", "", 2001},
		{10, false, "No year", "R", "US", "Drama", "en", 0},
		{11, true, "Show PG", "PG", "US", "Drama", "en", 2010},
		{12, false, "Stored NR", "NR", "US", "Drama", "en", 1980},
		{13, false, "Recent missing", "", "US", "Drama", "en", 2022},
		{14, false, "PG horror", "PG", "US", "Horror", "en", 2021},
	}
	titles := make([]core.Title, len(rows))
	for i, row := range rows {
		tt := core.Title{
			Key: core.MovieKey(row.id), Type: core.TypeMovie, Title: row.title,
			ContentRating: row.rating, Language: row.lang, Year: row.year, Released: released(row.year),
		}
		if row.show {
			tt.Key, tt.Type = core.ShowKey(row.id), core.TypeShow
		}
		if row.country != "" {
			tt.Countries = []string{row.country}
		}
		if row.genre != "" {
			tt.Genres = []string{row.genre}
		}
		// Browse only lists released titles. Keep year 0 with a real release
		// timestamp so unknown-year SQL is compared on a row Browse can see.
		if row.year == 0 {
			tt.Released = time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		if err = c.UpsertTitle(ctx, tt, true); err != nil {
			t.Fatal(err)
		}
		if row.year == 0 {
			if _, err = c.db.ExecContext(ctx, `UPDATE catalog_titles SET year=0 WHERE key=?`, int64(tt.Key)); err != nil {
				t.Fatal(err)
			}
			tt.Year = 0
		}
		titles[i] = tt
	}
	rule := func(field, op, raw string) core.CollectionRule {
		return core.CollectionRule{Field: field, Op: op, Value: json.RawMessage(raw)}
	}
	allow := `["G","PG","PG-13","PG13","M"]`
	assertHidden := func(specs []core.LibraryExclusion, want map[string]bool) {
		t.Helper()
		f, err := libraryfilter.Compile(specs)
		if err != nil {
			t.Fatal(err)
		}
		hidden := 0
		for i := range titles {
			got := f.Hidden(&titles[i])
			if got {
				hidden++
			}
			if exp, ok := want[titles[i].Title]; ok && got != exp {
				t.Errorf("%s: hidden=%v want %v under %s", titles[i].Title, got, exp, specs[0].Name)
			}
		}
		if len(want) != len(titles) {
			t.Fatalf("%s: expected a decision for every title", specs[0].Name)
		}
		c.UpdateConfig(config.Catalog{Exclusions: specs})
		page, err := c.Browse(ctx, core.BrowseQuery{Limit: len(titles)})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != len(titles)-hidden {
			t.Fatalf("%s: SQL total=%d in-memory hidden=%d", specs[0].Name, page.Total, hidden)
		}
		raw, err := c.PreviewVisibility(ctx, specs)
		if err != nil {
			t.Fatal(err)
		}
		if raw.(VisibilityPreview).Hidden != hidden {
			t.Fatalf("%s: preview hidden=%d in-memory=%d", specs[0].Name, raw.(VisibilityPreview).Hidden, hidden)
		}
	}
	all := func(hidden ...string) map[string]bool {
		want := map[string]bool{}
		for _, tt := range titles {
			want[tt.Title] = false
		}
		for _, name := range hidden {
			want[name] = true
		}
		return want
	}
	group := func(name, media, match string, rules ...core.CollectionRule) []core.LibraryExclusion {
		return []core.LibraryExclusion{{Name: name, MediaType: media, Match: match, Rules: rules}}
	}
	assertHidden(group("allowlist", "movie", "any", rule("contentRating", "notIn", allow)), all(
		"Blocked R", "Missing horror", "Missing comedy", "No year", "Stored NR", "Recent missing",
	))
	assertHidden(group("horror outside allowlist", "movie", "all",
		rule("contentRating", "notIn", allow), rule("genre", "in", `["Horror"]`)), all(
		"Blocked R", "Missing horror",
	))
	assertHidden(group("allowlist or India", "movie", "any",
		rule("contentRating", "notIn", allow), rule("country", "in", `["IN"]`)), all(
		"Blocked R", "Missing horror", "Missing comedy", "No year", "Stored NR", "Recent missing", "India PG",
	))
	assertHidden(group("known non-India", "mixed", "all", rule("country", "notIn", `["IN"]`)), all(
		"Allowed PG", "Allowed PG-13", "Allowed PG13", "Blocked R", "Missing horror", "Missing comedy",
		"No language", "No year", "Show PG", "Stored NR", "Recent missing", "PG horror",
	))
	assertHidden(group("not English", "mixed", "all", rule("language", "notIn", `["en"]`)), all(
		"India PG",
	))
	assertHidden(group("recent", "mixed", "all", rule("year", "gte", `2000`)), all(
		"India PG", "No country", "No language", "Show PG", "Recent missing", "PG horror",
	))
	assertHidden(group("only R", "movie", "any", rule("contentRating", "in", `["R"]`)), all(
		"Blocked R", "No year",
	))
	assertHidden(group("recent outside allowlist", "movie", "all",
		rule("year", "gte", `2000`), rule("contentRating", "notIn", allow)), all(
		"Recent missing",
	))
}

// Remote search summaries (movieToTitle / tvToTitle) omit certification.
// Visibility must use the stored content_rating when the catalogue has one,
// so a known allowed title is not excluded and a known blocked title is.
// A summary rating is kept only when the local row has none. No provider call.
func TestFilterVisibilityUsesKnownLocalContentRating(t *testing.T) {
	ctx := context.Background()
	specs := []core.LibraryExclusion{{
		Name: "Allowlist", MediaType: "movie", Match: "any",
		Rules: []core.CollectionRule{{Field: "contentRating", Op: "notIn", Value: json.RawMessage(`["G","PG","PG-13","PG13","M"]`)}},
	}}
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := New(db, nil, config.Catalog{Exclusions: specs}, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := time.Date(1974, 6, 1, 0, 0, 0, 0, time.UTC)
	put := func(id int64, rating string) {
		t.Helper()
		if err := c.UpsertTitle(ctx, core.Title{
			Key: core.MovieKey(id), Type: core.TypeMovie, Title: "Local " + rating,
			Year: 1974, Released: released, ContentRating: rating,
		}, true); err != nil {
			t.Fatal(err)
		}
	}
	put(1, "PG")
	put(2, "")
	put(3, "R")
	put(4, "PG-13")
	put(5, "PG")
	put(6, "")
	got := c.FilterVisibility(ctx, []core.Title{
		{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "bare allowed"},
		{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "bare missing"},
		{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "bare blocked"},
		{Key: core.MovieKey(4), Type: core.TypeMovie, Title: "bare teen"},
		{Key: core.MovieKey(5), Type: core.TypeMovie, Title: "summary disagrees", ContentRating: "R"},
		{Key: core.MovieKey(6), Type: core.TypeMovie, Title: "summary allowed", ContentRating: "PG"},
		{Key: core.MovieKey(999), Type: core.TypeMovie, Title: "not stored"},
	})
	if got == nil {
		t.Fatal("filter failed closed by returning nil")
	}
	want := map[core.RatingKey]string{
		core.MovieKey(1): "PG",
		core.MovieKey(4): "PG-13",
		core.MovieKey(5): "PG",
		core.MovieKey(6): "PG",
	}
	if len(got) != len(want) {
		t.Fatalf("kept %d titles: %+v", len(got), got)
	}
	for _, item := range got {
		rating, ok := want[item.Key]
		if !ok || item.ContentRating != rating {
			t.Fatalf("kept %+v", item)
		}
	}
}

// SQLHidden hides the same titles Hidden does. Facet rules are key sets.
func TestExclusionSQLMatchesFilter(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, err := New(db, nil, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	facets := []struct {
		countries, genres []string
		typ               core.MediaType
	}{
		{nil, nil, core.TypeMovie}, {[]string{"IN"}, nil, core.TypeMovie}, {[]string{"US", "IN"}, []string{"Drama"}, core.TypeShow},
		{[]string{"FR"}, []string{"Horror"}, core.TypeMovie}, {[]string{"JP"}, []string{"Animation", "Drama"}, core.TypeShow}, {nil, []string{"Horror"}, core.TypeShow},
	}
	var keys []core.RatingKey
	for i, f := range facets {
		key := core.MovieKey(int64(i + 1))
		if f.typ == core.TypeShow {
			key = core.ShowKey(int64(i + 1))
		}
		keys = append(keys, key)
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: f.typ, IDs: core.IDs{TMDB: key.TMDB()}, Title: "T", Released: now.AddDate(-1, 0, 0), Countries: f.countries, Genres: f.genres}, true); err != nil {
			t.Fatal(err)
		}
	}
	rule := func(field, op, raw string) core.CollectionRule {
		return core.CollectionRule{Field: field, Op: op, Value: []byte(raw)}
	}
	for name, specs := range map[string][]core.LibraryExclusion{
		"country in":       {{Name: "G", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("country", "in", `["IN","JP"]`)}}},
		"country notIn":    {{Name: "G", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("country", "notIn", `["US","FR"]`)}}},
		"genre any":        {{Name: "G", MediaType: "show", Match: "any", Rules: []core.CollectionRule{rule("genre", "in", `["Horror"]`), rule("country", "is", `"JP"`)}}},
		"genre isNot all":  {{Name: "G", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("genre", "isNot", `"Drama"`), rule("country", "in", `["FR","IN"]`)}}},
		"two groups mixed": {{Name: "G", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{rule("country", "in", `["IN"]`)}}, {Name: "G", MediaType: "show", Match: "all", Rules: []core.CollectionRule{rule("genre", "notIn", `["Drama"]`)}}},
	} {
		f, err := libraryfilter.Compile(specs)
		if err != nil {
			t.Fatal(name, err)
		}
		where, args := f.SQLHidden()
		var sqlHidden []string
		rows, err := db.QueryContext(ctx, `SELECT key FROM catalog_titles t WHERE `+where+` ORDER BY key`, args...)
		if err != nil {
			t.Fatal(name, err)
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			sqlHidden = append(sqlHidden, k)
		}
		rows.Close()
		var goHidden []string
		for _, key := range keys {
			title, err := c.StoredTitle(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if f.Hidden(title) {
				goHidden = append(goHidden, strconv.FormatInt(int64(key), 10))
			}
		}
		sort.Strings(goHidden)
		sort.Strings(sqlHidden)
		if strings.Join(sqlHidden, ",") != strings.Join(goHidden, ",") {
			t.Errorf("%s: SQL hides %v, filter hides %v", name, sqlHidden, goHidden)
		}
	}
}
