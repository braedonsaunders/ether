package experience

import (
	"errors"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestArrangeLayoutOrderAndDuplicateIDs(t *testing.T) {
	rows := []LayoutCandidate{{ID: "a", ItemCount: 2}, {ID: "b", ItemCount: 4, Purpose: core.RowPurpose("continue")}, {ID: "a", ItemCount: 7}}
	got, err := ArrangeLayout(rows, func(input []LayoutCandidate) ([]string, error) {
		if len(input) != 3 || input[1].ItemCount != 4 || input[1].Purpose != core.RowPurpose("continue") {
			t.Fatal("rendered counts not passed to layout")
		}
		return []string{"b", "missing", "a", "a"}, nil
	})
	if err != nil || !reflect.DeepEqual(got, []int{1, 2, 2}) {
		t.Fatalf("indices %v, error %v", got, err)
	}
}

func TestArrangeLayoutPropagatesFailure(t *testing.T) {
	failure := errors.New("layout failed")
	got, err := ArrangeLayout(nil, func([]LayoutCandidate) ([]string, error) { return nil, failure })
	if got != nil || !errors.Is(err, failure) {
		t.Fatalf("indices %v, error %v", got, err)
	}
}
