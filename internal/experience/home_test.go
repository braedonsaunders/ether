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

func TestCompleteHomeProducerOrder(t *testing.T) {
	var got []string
	add := func(s string) func() { return func() { got = append(got, s) } }
	CompleteHome(true, add("playlist"), add("discovery"), add("live"), add("selection"), add("layout"))
	if !reflect.DeepEqual(got, []string{"playlist", "discovery", "live", "selection", "layout"}) {
		t.Fatalf("order %v", got)
	}
}
