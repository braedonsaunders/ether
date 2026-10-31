package app

import (
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestCandidateProjectionDropsWireContext(t *testing.T) {
	source := []core.Hub{{ID: "same", Title: "first", Context: "hub.first", Type: core.TypeMovie, Style: "hero", Promoted: true, Items: []core.Title{{Key: core.MovieKey(1)}}, More: true, Key: core.CollectionKey(4), ItemCount: 7}, {ID: "same", Title: "second", Context: "hub.second", Type: core.TypeShow}}
	rows, err := rankedCandidates(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reflect.TypeOf(rows[0]).FieldByName("Context"); ok {
		t.Fatal("wire context leaked into candidate")
	}
	want := experience.HubRows(source)
	got := experience.CandidateRows(rows)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hub rows=%+v candidate rows=%+v", want, got)
	}
	if got[0].Purpose != frontend.RowCollection || got[1].Purpose != frontend.RowRecommendation {
		t.Fatalf("semantic purposes changed: %+v", got)
	}
}
