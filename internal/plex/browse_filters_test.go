package plex

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func matchStringFilter(cand string, sf core.StringFilter) bool {
	c := strings.ToLower(cand)
	v := strings.ToLower(sf.Value)
	switch sf.Op {
	case "==":
		return c == v
	case "!==":
		return c != v
	case "!=":
		return !strings.Contains(c, v)
	case "<=":
		return strings.HasPrefix(c, v)
	case ">=":
		return strings.HasSuffix(c, v)
	default:
		return strings.Contains(c, v)
	}
}

func fakeFilterMatch(t core.Title, f core.Filter) bool {
	if f.Type != "" && t.Type != f.Type {
		return false
	}
	if t.Adult {
		return false
	}
	if t.Type == core.TypeEpisode && (t.Released.IsZero() || t.Released.After(fixedNow)) {
		return false
	}
	if f.KnownAddedDate && t.DateAdded(fixedNow).IsZero() {
		return false
	}
	if f.AddedAfter > 0 || f.AddedBefore > 0 {
		added := unixOrZero(t.DateAdded(fixedNow))
		if added <= 0 || (f.AddedAfter > 0 && added < f.AddedAfter) || (f.AddedBefore > 0 && added > f.AddedBefore) {
			return false
		}
	}
	if f.UpdatedAfter > 0 || f.UpdatedBefore > 0 {
		updated := unixOrZero(t.UpdatedAt)
		if updated <= 0 || (f.UpdatedAfter > 0 && updated < f.UpdatedAfter) || (f.UpdatedBefore > 0 && updated > f.UpdatedBefore) {
			return false
		}
	}
	if len(f.Genres) > 0 {
		match := false
		for _, g := range f.Genres {
			if slices.Contains(t.Genres, g) {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	for _, g := range f.ExcludeGenres {
		for _, tg := range t.Genres {
			if strings.EqualFold(g, tg) {
				return false
			}
		}
	}
	if f.YearFrom > 0 && t.Year < f.YearFrom {
		return false
	}
	if f.YearTo > 0 && (t.Year <= 0 || t.Year > f.YearTo) {
		return false
	}
	if len(f.Years) > 0 && !slices.Contains(f.Years, t.Year) {
		return false
	}
	if len(f.ExcludeYears) > 0 && (t.Year <= 0 || slices.Contains(f.ExcludeYears, t.Year)) {
		return false
	}
	if f.MinRating > 0 && t.Rating < f.MinRating {
		return false
	}
	if f.MaxRating > 0 && (t.Rating <= 0 || t.Rating > f.MaxRating) {
		return false
	}
	if len(f.ExcludeRatings) > 0 && (t.Rating <= 0 || slices.Contains(f.ExcludeRatings, t.Rating)) {
		return false
	}
	if f.MaxRuntime > 0 && (t.Runtime <= 0 || t.Runtime > f.MaxRuntime) {
		return false
	}
	if f.MinRuntime > 0 && t.Runtime < f.MinRuntime {
		return false
	}
	if len(f.ExcludeRuntimes) > 0 && (t.Runtime <= 0 || slices.Contains(f.ExcludeRuntimes, t.Runtime)) {
		return false
	}
	if len(f.Countries) > 0 {
		match := false
		for _, c := range f.Countries {
			if slices.Contains(t.Countries, c) {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	for _, c := range f.ExcludeCountries {
		if slices.Contains(t.Countries, c) {
			return false
		}
	}
	if len(f.ContentRatings) > 0 && !slices.Contains(f.ContentRatings, t.ContentRating) {
		return false
	}
	if len(f.ExcludeContentRatings) > 0 && slices.Contains(f.ExcludeContentRatings, t.ContentRating) {
		return false
	}
	if f.Collection > 0 && len(f.Collections) == 0 && t.CollectionID != f.Collection {
		return false
	}
	if len(f.Collections) > 0 && !slices.Contains(f.Collections, t.CollectionID) {
		return false
	}
	if len(f.ExcludeCollections) > 0 && slices.Contains(f.ExcludeCollections, t.CollectionID) {
		return false
	}
	hasPerson := func(id int64, kind string) bool {
		check := func(ps []core.Person) bool {
			for _, p := range ps {
				if p.TMDB == id {
					return true
				}
			}
			return false
		}
		switch kind {
		case "cast":
			return check(t.Cast)
		case "director":
			return check(t.Directors)
		case "writer":
			return check(t.Writers)
		case "crew":
			return check(t.Crew)
		default:
			return check(t.Cast) || check(t.Directors) || check(t.Writers) || check(t.Crew)
		}
	}
	if f.Person > 0 && len(f.Credits) == 0 && !hasPerson(f.Person, "") {
		return false
	}
	for _, cf := range f.Credits {
		anyMatch := false
		for _, pid := range cf.People {
			if hasPerson(pid, cf.Kind) {
				anyMatch = true
				break
			}
		}
		if cf.Exclude && anyMatch {
			return false
		}
		if !cf.Exclude && !anyMatch {
			return false
		}
	}
	if f.Studio != "" && len(f.StudioFilters) == 0 && !slices.Contains(t.Studios, f.Studio) {
		return false
	}
	for _, sf := range f.StudioFilters {
		switch sf.Op {
		case "!=", "!==":
			for _, st := range t.Studios {
				if !matchStringFilter(st, sf) {
					return false
				}
			}
		default:
			matched := false
			for _, st := range t.Studios {
				if matchStringFilter(st, sf) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	if f.Query != "" && !strings.Contains(strings.ToLower(t.Title), strings.ToLower(f.Query)) {
		return false
	}
	for _, tf := range f.TitleFilters {
		if !matchStringFilter(t.Title, tf) {
			return false
		}
	}
	for _, rf := range f.ReleasedFilters {
		if t.Released.IsZero() {
			return false
		}
		ts := t.Released.Unix()
		const day = 86400
		switch rf.Op {
		case "<<":
			if ts >= rf.Unix {
				return false
			}
		case "<=":
			end := rf.Unix
			if rf.SpanDay {
				end += day - 1
			}
			if ts > end {
				return false
			}
		case ">>":
			if ts <= rf.Unix {
				return false
			}
		case ">=":
			if ts < rf.Unix {
				return false
			}
		case "!=", "!==":
			if rf.SpanDay {
				if ts >= rf.Unix && ts < rf.Unix+day {
					return false
				}
			} else if ts == rf.Unix {
				return false
			}
		default:
			if rf.SpanDay {
				if ts < rf.Unix || ts >= rf.Unix+day {
					return false
				}
			} else if ts != rf.Unix {
				return false
			}
		}
	}
	return true
}

func browseTitles(t *testing.T, f *fixture, path string) []string {
	t.Helper()
	mc := decodeJSON(t, f.get(t, path, jsonAccept...))
	raw, ok := mc["Metadata"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any)["title"].(string))
	}
	return out
}

func TestBrowseFilterNegationAndAdvertisedOperators(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	// Seed rich attributes on alien so both movies have distinct values for every advertised field.
	alienRich := alien
	alienRich.Released = time.Date(1979, 5, 25, 0, 0, 0, 0, time.UTC)
	alienRich.Studios = []string{"Brandywine Productions"}
	alienRich.Countries = []string{"United Kingdom"}
	alienRich.ContentRating = "PG-13"
	alienRich.Rating = 8.5
	alienRich.CollectionID = 8091
	alienRich.Cast = []core.Person{{TMDB: 10205, Name: "Sigourney Weaver", Role: "Ripley"}}
	alienRich.Directors = []core.Person{{TMDB: 578, Name: "Ridley Scott"}}
	alienRich.Writers = []core.Person{{TMDB: 5045, Name: "Dan O'Bannon"}}
	f.catalog.titles[alienRich.Key] = alienRich

	fcRich := fightClub
	fcRich.CollectionID = 10
	f.catalog.titles[fcRich.Key] = fcRich

	cases := []struct {
		name string
		path string
		want []string
	}{
		// Genre (= and !=, facet key and title)
		{"genre is Drama", "/library/sections/1/all?genre=Drama", []string{"Fjord Harbor"}},
		{"genre is not Drama", "/library/sections/1/all?genre!=Drama", []string{"Alien"}},
		{"genre is not facet 18", "/library/sections/1/all?genre!=18", []string{"Alien"}},
		{"genre is not Drama,Horror", "/library/sections/1/all?genre!=Drama,Horror", nil},

		// Country (= and !=)
		{"country is US", "/library/sections/1/all?country=United+States+of+America", []string{"Fjord Harbor"}},
		{"country is not US", "/library/sections/1/all?country!=United+States+of+America", []string{"Alien"}},

		// ContentRating (= and !=)
		{"contentRating is R", "/library/sections/1/all?contentRating=R", []string{"Fjord Harbor"}},
		{"contentRating is not R", "/library/sections/1/all?contentRating!=R", []string{"Alien"}},

		// Collection (= and !=)
		{"collection is 8091", "/library/sections/1/all?collection=8091", []string{"Alien"}},
		{"collection is not 8091", "/library/sections/1/all?collection!=8091", []string{"Fjord Harbor"}},

		// Director, Writer, Actor (= and !=, scoped by credit kind)
		{"director is 7467", "/library/sections/1/all?director=7467", []string{"Fjord Harbor"}},
		{"director is not 7467", "/library/sections/1/all?director!=7467", []string{"Alien"}},
		{"writer is 5045", "/library/sections/1/all?writer=5045", []string{"Alien"}},
		{"writer is not 5045", "/library/sections/1/all?writer!=5045", []string{"Fjord Harbor"}},
		{"actor is 287", "/library/sections/1/all?actor=287", []string{"Fjord Harbor"}},
		{"actor is not 287", "/library/sections/1/all?actor!=287", []string{"Alien"}},
		{"director id queried as actor does not match", "/library/sections/1/all?actor=7467", nil},

		// Title string operators (=, !=, ==, !==, <=, >=)
		{"title contains Fjord", "/library/sections/1/all?title=Fjord", []string{"Fjord Harbor"}},
		{"title does not contain Fjord", "/library/sections/1/all?title!=Fjord", []string{"Alien"}},
		{"title exact match", "/library/sections/1/all?title==Alien", []string{"Alien"}},
		{"title exact partial fails", "/library/sections/1/all?title==Fjord", nil},
		{"title not exact match", "/library/sections/1/all?title!==Alien", []string{"Fjord Harbor"}},
		{"title begins with Fjord", "/library/sections/1/all?title<=Fjord", []string{"Fjord Harbor"}},
		{"title ends with Harbor", "/library/sections/1/all?title>=Harbor", []string{"Fjord Harbor"}},

		// Studio string operators (=, !=, ==, !==, <=, >=)
		{"studio contains Northlight", "/library/sections/1/all?studio=Northlight", []string{"Fjord Harbor"}},
		{"studio does not contain Northlight", "/library/sections/1/all?studio!=Northlight", []string{"Alien"}},
		{"studio exact", "/library/sections/1/all?studio==Brandywine+Productions", []string{"Alien"}},
		{"studio not exact", "/library/sections/1/all?studio!==Brandywine+Productions", []string{"Fjord Harbor"}},
		{"studio begins with Brandywine", "/library/sections/1/all?studio<=Brandywine", []string{"Alien"}},
		{"studio ends with Pictures", "/library/sections/1/all?studio>=Pictures", []string{"Fjord Harbor"}},

		// Year & Decade (=, !=, >>=, <<=)
		{"year is 1999", "/library/sections/1/all?year=1999", []string{"Fjord Harbor"}},
		{"year is 1979,1999", "/library/sections/1/all?year=1979,1999", []string{"Alien", "Fjord Harbor"}},
		{"year is not 1999", "/library/sections/1/all?year!=1999", []string{"Alien"}},
		{"year greater than 1979", "/library/sections/1/all?year>>=1979", []string{"Fjord Harbor"}},
		{"year less than 1999", "/library/sections/1/all?year<<=1999", []string{"Alien"}},
		{"decade is 1990", "/library/sections/1/all?decade=1990", []string{"Fjord Harbor"}},
		{"decade is not 1990", "/library/sections/1/all?decade!=1990", []string{"Alien"}},
		{"decade greater than 1970", "/library/sections/1/all?decade>>=1970", []string{"Fjord Harbor"}},
		{"decade less than 1990", "/library/sections/1/all?decade<<=1990", []string{"Alien"}},

		// Release Date (originallyAvailableAt <<= and >>=)
		{"originallyAvailableAt before 1990-01-01", "/library/sections/1/all?originallyAvailableAt<<=1990-01-01", []string{"Alien"}},
		{"originallyAvailableAt after 1990-01-01", "/library/sections/1/all?originallyAvailableAt>>=1990-01-01", []string{"Fjord Harbor"}},
		{"originallyAvailableAt relative -30y", "/library/sections/1/all?originallyAvailableAt>>=-30y", []string{"Fjord Harbor"}},
		{"originallyAvailableAt is 1999-10-15", "/library/sections/1/all?originallyAvailableAt=1999-10-15", []string{"Fjord Harbor"}},
		{"originallyAvailableAt is not 1999-10-15", "/library/sections/1/all?originallyAvailableAt!=1999-10-15", []string{"Alien"}},

		// Duration (ms on wire)
		{"duration is 8340000", "/library/sections/1/all?duration=8340000", []string{"Fjord Harbor"}},
		{"duration is not 8340000", "/library/sections/1/all?duration!=8340000", []string{"Alien"}},
		{"duration greater than 7200000", "/library/sections/1/all?duration>>=7200000", []string{"Fjord Harbor"}},
		{"duration less than 7200000", "/library/sections/1/all?duration<<=7200000", []string{"Alien"}},

		// AudienceRating (=, !=, >>=, <<=)
		{"audienceRating greater than 8.45", "/library/sections/1/all?audienceRating>>=8.45", []string{"Alien"}},
		{"audienceRating less than 8.45", "/library/sections/1/all?audienceRating<<=8.45", []string{"Fjord Harbor"}},
		{"audienceRating is 8.5", "/library/sections/1/all?audienceRating=8.5", []string{"Alien"}},
		{"audienceRating is not 8.5", "/library/sections/1/all?audienceRating!=8.5", []string{"Fjord Harbor"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := browseTitles(t, f, tc.path)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("%s: got %v, want %v (filter=%+v)", tc.path, got, tc.want, f.catalog.lastQ.Filter)
			}
		})
	}

	// Boolean watch-state filters: unwatched and inProgress (= and !=).
	// Mark Alien watched, and Fjord Harbor in-progress (unwatched with offset).
	f.state.set(ownerUser.ID, core.WatchStatus{Key: alienRich.Key, ViewCount: 1, LastViewedAt: fixedNow})
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fcRich.Key, ViewOffset: 10 * time.Minute, ViewCount: 0})

	watchCases := []struct {
		name string
		path string
		want []string
	}{
		{"unwatched=1", "/library/sections/1/all?unwatched=1", []string{"Fjord Harbor"}},
		{"unwatched!=1 (watched)", "/library/sections/1/all?unwatched!=1", []string{"Alien"}},
		{"unwatched=0 (watched)", "/library/sections/1/all?unwatched=0", []string{"Alien"}},
		{"unwatched!=0 (unwatched)", "/library/sections/1/all?unwatched!=0", []string{"Fjord Harbor"}},
		{"inProgress=1", "/library/sections/1/all?inProgress=1", []string{"Fjord Harbor"}},
		{"inProgress!=1", "/library/sections/1/all?inProgress!=1", []string{"Alien"}},
		{"inProgress=0", "/library/sections/1/all?inProgress=0", []string{"Alien"}},
		{"inProgress!=0", "/library/sections/1/all?inProgress!=0", []string{"Fjord Harbor"}},
		{"viewCount!=0", "/library/sections/1/all?viewCount!=0", []string{"Alien"}},
	}
	for _, tc := range watchCases {
		t.Run(tc.name, func(t *testing.T) {
			got := browseTitles(t, f, tc.path)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("%s: got %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
