package plex

import (
	"context"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

type renderedLayout struct {
	core.Recommender
	core.HomeLayoutSource
	got []core.Hub
}

func (r *renderedLayout) FinalizeHome(_ context.Context, _ *core.User, _ core.HubContext, _ string, rows []core.Hub) ([]core.Hub, error) {
	r.got = rows
	return []core.Hub{rows[1], rows[0]}, nil
}
func TestFinalLayoutReceivesRenderedAvailability(t *testing.T) {
	r := &renderedLayout{}
	s := &Server{d: Deps{Recommender: r}}
	rows := []model.Hub{
		{HubIdentifier: "movie.inprogress.1", Size: 0},
		{HubIdentifier: "movie.top10", Size: 10, Key: "/original/key", Metadata: []model.Metadata{{Title: "Original metadata"}}},
	}
	got := s.finalizeHomeRows(context.Background(), &Request{User: &core.User{ID: 1}}, core.HubContext{}, "movies", rows)
	if r.got[0].ItemCount != 0 || r.got[1].ItemCount != 10 {
		t.Fatalf("availability: %+v", r.got)
	}
	if !reflect.DeepEqual(got, []model.Hub{rows[1], rows[0]}) {
		t.Fatal("layout changed original Plex metadata")
	}
}

func TestHomeRowAssemblyStableForSameCandidates(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.Recommender = &fakeRecommender{home: []core.Hub{
		{ID: "home.top10.today", Title: "Top 10", Type: core.TypeMovie, Promoted: true, Items: []core.Title{fightClub}},
		{ID: "home.shortontime", Title: "Short on Time", Type: core.TypeMovie, Promoted: true, Items: []core.Title{alien}},
	}}
	for _, path := range []string{"/hubs", "/hubs/promoted"} {
		first := hubIDs(t, f.get(t, path, "Accept", "application/json").Body.Bytes())
		second := hubIDs(t, f.get(t, path, "Accept", "application/json").Body.Bytes())
		want := []string{"home.continue", "home.top10.today", "home.shortontime"}
		if !reflect.DeepEqual(first, want) {
			t.Fatalf("%s: unexpected row order: %v", path, first)
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("%s: row order changed: %v != %v", path, first, second)
		}
	}
}
