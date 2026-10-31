package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"testing"
	"time"
)

func TestAddedDateIgnoresPublicationAndObservation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	released := time.Unix(1072915200, 0).UTC() // 2004-01-01
	observed := time.Unix(1704067200, 0).UTC() // 2024-01-01
	for _, typ := range []core.MediaType{core.TypeMovie, core.TypeEpisode, core.TypeShow, core.TypeSeason} {
		title := core.Title{Type: typ, Released: released, AddedAt: observed, UpdatedAt: now}
		if got := title.DateAdded(now); !got.Equal(released) {
			t.Fatalf("%s: catalogue date %s replaced release %s", typ, got, released)
		}
		title.Released, title.AddedAt = time.Time{}, time.Time{}
		if !title.DateAdded(now).IsZero() {
			t.Fatal("invented release date from refresh time")
		}
		title.Released, title.AddedAt = now.Add(time.Hour), now.Add(time.Hour)
		if !title.DateAdded(now).IsZero() {
			t.Fatal("future date counted as added")
		}
	}
}

func TestDateAddedSortAcrossTypesAndChildren(t *testing.T) {
	released, observed := time.Unix(1072915200, 0).UTC(), time.Unix(1704067200, 0).UTC()
	s := &Service{}
	items := []core.Title{{Key: 1, Type: core.TypeMovie, Released: released, AddedAt: observed}, {Key: 2, Type: core.TypeEpisode, Released: released.Add(time.Hour)}, {Key: 3, Type: core.TypeShow, Released: released, AddedAt: observed}}
	if err := s.sortTitles(context.Background(), items, frontend.ItemQuery{Sort: core.SortAdded, Desc: true}); err != nil {
		t.Fatal(err)
	}
	if items[0].Key != 2 || items[1].Key != 1 || items[2].Key != 3 {
		t.Fatalf("order: %+v", items)
	}
	show := items[2]
	if !show.DateAdded(time.Unix(1767225600, 0)).Equal(released) {
		t.Fatalf("show sorted by catalogue date: %s", show.DateAdded(time.Unix(1767225600, 0)))
	}
}

func TestVirtualSeasonAddedSortKeepsDerivedAirDate(t *testing.T) {
	s := &Service{d: ServiceDeps{AddedTimes: func(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error) {
		return map[core.RatingKey]time.Time{}, nil
	}}}
	items := []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie, Released: time.Unix(946684800, 0).UTC()}, {Key: core.SeasonKey(10, 9000), Type: core.TypeSeason, AddedAt: time.Unix(1072915200, 0).UTC()}}
	if err := s.sortTitles(context.Background(), items, frontend.ItemQuery{Sort: core.SortAdded, Desc: true}); err != nil {
		t.Fatal(err)
	}
	if items[0].Key != core.SeasonKey(10, 9000) || items[0].AddedAt.Unix() != 1072915200 {
		t.Fatalf("virtual air date lost: %+v", items)
	}
}
