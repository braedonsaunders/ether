package experience

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type neutralLayoutTestSource struct {
	rows []core.NeutralHomeRow
	err  error
}

func (s neutralLayoutTestSource) FinalizeNeutralHome(context.Context, *core.User, core.HomeSituation, string, []core.NeutralHomeRow) ([]core.NeutralHomeRow, error) {
	return s.rows, s.err
}

func TestArrangeNeutralHomeStableSidecars(t *testing.T) {
	rows := []core.NeutralHomeRow{{ID: "a", Title: "first"}, {ID: "b"}, {ID: "a", Title: "last"}}
	indices, err := ArrangeNeutralHome(context.Background(), neutralLayoutTestSource{rows: []core.NeutralHomeRow{{ID: "b"}, {ID: "missing"}, {ID: "a"}, {ID: "a"}}}, &core.User{}, core.HomeSituation{}, "home", rows)
	if err != nil || !reflect.DeepEqual(indices, []int{1, 2, 2}) {
		t.Fatalf("indices %v error %v", indices, err)
	}
	failure := errors.New("layout unavailable")
	if _, err := ArrangeNeutralHome(context.Background(), neutralLayoutTestSource{err: failure}, &core.User{}, core.HomeSituation{}, "home", rows); !errors.Is(err, failure) {
		t.Fatalf("layout error: %v", err)
	}
}

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
