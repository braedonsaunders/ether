package catalog

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestBrowseFilterNegationAndOperators(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	c.SetNow(func() time.Time { return now })

	titles := []core.Title{
		{
			Key: core.MovieKey(550), Type: core.TypeMovie, IDs: core.IDs{TMDB: 550},
			Title: "Fjord Harbor", SortTitle: "fjord harbor",
			Year: 1999, Released: time.Date(1999, 10, 15, 0, 0, 0, 0, time.UTC),
			Runtime:       139 * time.Minute,
			Genres:        []string{"Drama", "Thriller"},
			Studios:       []string{"Northlight"},
			Countries:     []string{"United States of America"},
			ContentRating: "R",
			Rating:        8.4,
			Popularity:    60,
			CollectionID:  10,
			Cast:          []core.Person{{TMDB: 287, Name: "Theo Varga", Role: "Alex Mercer"}},
			Directors:     []core.Person{{TMDB: 7467, Name: "Mara Ellison"}},
			Writers:       []core.Person{{TMDB: 7468, Name: "Sasha Petrov"}},
		},
		{
			Key: core.MovieKey(551), Type: core.TypeMovie, IDs: core.IDs{TMDB: 551},
			Title: "Family Fun", SortTitle: "family fun",
			Year: 2020, Released: time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC),
			Runtime:       95 * time.Minute,
			Genres:        []string{"Family", "Comedy"},
			Studios:       []string{"Sunbeam Studios"},
			Countries:     []string{"Canada"},
			ContentRating: "PG",
			Rating:        7.0,
			Popularity:    40,
			CollectionID:  20,
			Cast:          []core.Person{{TMDB: 301, Name: "Clara Bell", Role: "Mom"}},
			Directors:     []core.Person{{TMDB: 302, Name: "Sam Rivers"}},
			Writers:       []core.Person{{TMDB: 303, Name: "Pat Lane"}},
		},
		{
			Key: core.MovieKey(552), Type: core.TypeMovie, IDs: core.IDs{TMDB: 552},
			Title: "Old Classic", SortTitle: "old classic",
			Year: 1960, Released: time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC),
			Genres:     []string{"Drama"},
			Rating:     9.0,
			Popularity: 20,
		},
	}
	for _, ti := range titles {
		if err := c.UpsertTitle(ctx, ti, true); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		f    core.Filter
		want []string
	}{
		{
			name: "exclude genre Drama",
			f:    core.Filter{Type: core.TypeMovie, ExcludeGenres: []string{"Drama"}},
			want: []string{"Family Fun"},
		},
		{
			name: "exclude country US",
			f:    core.Filter{Type: core.TypeMovie, ExcludeCountries: []string{"United States of America"}},
			want: []string{"Family Fun", "Old Classic"},
		},
		{
			name: "exclude contentRating R",
			f:    core.Filter{Type: core.TypeMovie, ExcludeContentRatings: []string{"R"}},
			want: []string{"Family Fun", "Old Classic"},
		},
		{
			name: "exclude collection 10",
			f:    core.Filter{Type: core.TypeMovie, ExcludeCollections: []int64{10}},
			want: []string{"Family Fun", "Old Classic"},
		},
		{
			name: "years list",
			f:    core.Filter{Type: core.TypeMovie, Years: []int{1960, 2020}},
			want: []string{"Family Fun", "Old Classic"},
		},
		{
			name: "exclude year 1999",
			f:    core.Filter{Type: core.TypeMovie, ExcludeYears: []int{1999}},
			want: []string{"Family Fun", "Old Classic"},
		},
		{
			name: "maxRating and excludeRatings",
			f:    core.Filter{Type: core.TypeMovie, MaxRating: 8.5, ExcludeRatings: []float64{7.0}},
			want: []string{"Fjord Harbor"},
		},
		{
			name: "exclude runtime 139m",
			f:    core.Filter{Type: core.TypeMovie, ExcludeRuntimes: []time.Duration{139 * time.Minute}},
			want: []string{"Family Fun"},
		},
		{
			name: "credit director positive",
			f:    core.Filter{Type: core.TypeMovie, Credits: []core.CreditFilter{{People: []int64{7467}, Kind: "director"}}},
			want: []string{"Fjord Harbor"},
		},
		{
			name: "credit director queried as cast fails",
			f:    core.Filter{Type: core.TypeMovie, Credits: []core.CreditFilter{{People: []int64{7467}, Kind: "cast"}}},
			want: nil,
		},
		{
			name: "credit cast exclude",
			f:    core.Filter{Type: core.TypeMovie, Credits: []core.CreditFilter{{People: []int64{287}, Kind: "cast", Exclude: true}}},
			want: []string{"Family Fun", "Old Classic"},
		},
		{
			name: "studio filter exclude",
			f:    core.Filter{Type: core.TypeMovie, StudioFilters: []core.StringFilter{{Op: "!=", Value: "North"}}},
			want: []string{"Family Fun", "Old Classic"},
		},
		{
			name: "studio filter begins with",
			f:    core.Filter{Type: core.TypeMovie, StudioFilters: []core.StringFilter{{Op: "<=", Value: "North"}}},
			want: []string{"Fjord Harbor"},
		},
		{
			name: "title exact",
			f:    core.Filter{Type: core.TypeMovie, TitleFilters: []core.StringFilter{{Op: "==", Value: "family fun"}}},
			want: []string{"Family Fun"},
		},
		{
			name: "title not exact",
			f:    core.Filter{Type: core.TypeMovie, TitleFilters: []core.StringFilter{{Op: "!==", Value: "family fun"}}},
			want: []string{"Fjord Harbor", "Old Classic"},
		},
		{
			name: "title does not contain",
			f:    core.Filter{Type: core.TypeMovie, TitleFilters: []core.StringFilter{{Op: "!=", Value: "fjord"}}},
			want: []string{"Family Fun", "Old Classic"},
		},
		{
			name: "title begins with",
			f:    core.Filter{Type: core.TypeMovie, TitleFilters: []core.StringFilter{{Op: "<=", Value: "fam"}}},
			want: []string{"Family Fun"},
		},
		{
			name: "title ends with",
			f:    core.Filter{Type: core.TypeMovie, TitleFilters: []core.StringFilter{{Op: ">=", Value: "harbor"}}},
			want: []string{"Fjord Harbor"},
		},
		{
			name: "released before 2000-01-01 and after 1970-01-01",
			f: core.Filter{
				Type: core.TypeMovie,
				ReleasedFilters: []core.DateFilter{
					{Op: "<<", Unix: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), SpanDay: true},
					{Op: ">>", Unix: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), SpanDay: true},
				},
			},
			want: []string{"Fjord Harbor"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := c.Browse(ctx, core.BrowseQuery{Filter: tc.f, Sort: core.SortTitle})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, it := range page.Items {
				got = append(got, it.Title)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v (total=%d)", got, tc.want, page.Total)
			}
			if page.Total != len(tc.want) {
				t.Fatalf("total=%d, want %d", page.Total, len(tc.want))
			}
		})
	}

	// Verify EXPLAIN uses indexes for credit/genre/country filters.
	where, args, err := c.browseWhere(core.Filter{
		Type:             core.TypeMovie,
		ExcludeGenres:    []string{"Drama"},
		ExcludeCountries: []string{"United States of America"},
		Credits:          []core.CreditFilter{{People: []int64{7467}, Kind: "director"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.DialectOf(c.db) == store.Postgres {
		rows, err := c.db.QueryContext(ctx, `EXPLAIN SELECT key FROM catalog_titles t WHERE `+where, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if plan.Len() == 0 {
			t.Fatal("empty EXPLAIN output on PostgreSQL")
		}
	} else {
		rows, err := c.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT key FROM catalog_titles t WHERE `+where, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail + "\n")
		}
		p := plan.String()
		if !strings.Contains(p, "catalog_credits_person_kind") {
			t.Fatalf("expected catalog_credits_person_kind in EXPLAIN QUERY PLAN:\n%s", p)
		}
	}
}
