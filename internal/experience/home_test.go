package experience

import (
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestAssembleHomePlacementAndDeduplication(t *testing.T) {
	candidates := []HomeCandidate{{Index: 0, Purpose: core.RowPurpose("recommendation"), Promoted: true, InLibraries: true}, {Index: 1, Purpose: core.RowPurpose("continue")}, {Index: 2, Purpose: core.RowPurpose("recommendation"), Promoted: true, InLibraries: true}, {Index: 3, Purpose: core.RowPurpose("recommendation"), InLibraries: true}, {Index: 4, Purpose: core.RowPurpose("recommendation"), Promoted: true}}
	var got []int
	AssembleHome(candidates, true, true, "continue", func() { got = append(got, -1) }, func(c HomeCandidate) string { return "same" }, func(c HomeCandidate) { got = append(got, c.Index) })
	if !reflect.DeepEqual(got, []int{0, -1}) {
		t.Fatalf("order/dedupe: %v", got)
	}
	got = nil
	AssembleHome(nil, true, false, "continue", func() { got = append(got, -1) }, func(HomeCandidate) string { return "" }, func(HomeCandidate) {})
	if !reflect.DeepEqual(got, []int{-1}) {
		t.Fatalf("default continue: %v", got)
	}
}

func TestHomeAssemblyKeepsPaintFactsWithFilteredRows(t *testing.T) {
	var seen []string
	assembly := HomeAssembly[string]{
		Render:   func(h core.Hub) string { return h.ID },
		Identity: func(row string) string { return row },
		Report: func(row string) (core.Hub, core.NeutralHomeRow) {
			return core.Hub{ID: row, ItemCount: 3}, core.NeutralHomeRow{ID: row, ItemCount: 3}
		},
		Continue:    func() (core.Hub, string) { return core.Hub{}, "" },
		Playlists:   func() (string, bool) { return "", false },
		Discovery:   func() []core.Hub { return nil },
		Live:        func() (string, bool) { return "", false },
		InLibraries: func(core.Hub) bool { return true },
		Keep:        func(row string) bool { return row != "drop" },
		Layout: func(rows []string, hubs []core.Hub, neutral []core.NeutralHomeRow) []string {
			if len(rows) != 1 || rows[0] != "keep" || hubs[0].ID != "keep" || hubs[0].ItemCount != 3 || neutral[0].ItemCount != 3 {
				t.Fatalf("facts rows=%v hubs=%+v neutral=%+v", rows, hubs, neutral)
			}
			seen = append(seen, rows...)
			return rows
		},
	}
	got, served := assembly.Rows([]core.Hub{{ID: "keep", Promoted: true}, {ID: "drop", Promoted: true}}, false, false, false, func(core.Hub) core.RowPurpose { return "recommendation" })
	if !reflect.DeepEqual(got, []string{"keep"}) || served["keep"].ID != "keep" || served["drop"].ID != "drop" || len(seen) != 1 {
		t.Fatalf("rows %v served %+v seen %v", got, served, seen)
	}
}

func TestHomeAssemblyCreditsFirstDuplicateThenDiscovery(t *testing.T) {
	// main 4588b9893: a repeated recommender identifier is not rendered again,
	// so the first arm stays in the impression map; a later discovery hub with
	// that id overwrites it; playlists and live are not inserted.
	assembly := func(discovery []core.Hub) HomeAssembly[string] {
		return HomeAssembly[string]{
			Render:   func(h core.Hub) string { return h.ID },
			Identity: func(row string) string { return row },
			Continue: func() (core.Hub, string) {
				return core.Hub{ID: "home.continue", Items: []core.Title{{Key: 9}}}, "home.continue"
			},
			Playlists:   func() (string, bool) { return "home.playlists", true },
			Discovery:   func() []core.Hub { return discovery },
			Live:        func() (string, bool) { return "livetv", true },
			InLibraries: func(core.Hub) bool { return true },
		}
	}
	first := core.Hub{ID: "home.trending", Promoted: true, Items: []core.Title{{Key: 1}}}
	second := core.Hub{ID: "home.trending", Promoted: true, Items: []core.Title{{Key: 2}}}
	_, served := assembly(nil).Rows([]core.Hub{first, second}, true, false, true, func(core.Hub) core.RowPurpose { return "recommendation" })
	if len(served["home.trending"].Items) != 1 || served["home.trending"].Items[0].Key != 1 {
		t.Fatalf("first arm credit %+v", served["home.trending"])
	}
	if len(served["home.continue"].Items) != 1 || served["home.continue"].Items[0].Key != 9 {
		t.Fatalf("continue %+v", served["home.continue"])
	}
	if _, ok := served["home.playlists"]; ok {
		t.Fatal("playlist credited")
	}
	if _, ok := served["livetv"]; ok {
		t.Fatal("live credited")
	}
	_, served = assembly([]core.Hub{{ID: "home.trending", Items: []core.Title{{Key: 3}}}}).Rows([]core.Hub{first, second}, true, false, true, func(core.Hub) core.RowPurpose { return "recommendation" })
	if len(served["home.trending"].Items) != 1 || served["home.trending"].Items[0].Key != 3 {
		t.Fatalf("discovery overwrite %+v", served["home.trending"])
	}
}

func TestCompleteHomeProducerOrder(t *testing.T) {
	var got []string
	add := func(s string) func() { return func() { got = append(got, s) } }
	CompleteHome(true, add("playlist"), add("discovery"), add("live"), add("selection"), add("layout"))
	if !reflect.DeepEqual(got, []string{"playlist", "discovery", "live", "selection", "layout"}) {
		t.Fatalf("order %v", got)
	}
}
