package paging

import (
	"math"
	"testing"
)

func TestBounds(t *testing.T) {
	tests := []struct{ total, offset, limit, start, end int }{{5, 1, 2, 1, 3}, {5, -1, 2, 0, 2}, {5, 10, 2, 5, 5}, {5, 1, -1, 1, 5}, {5, 1, 0, 1, 1}, {5, 1, math.MaxInt, 1, 5}, {-1, 0, 3, 0, 0}}
	for _, v := range tests {
		a, b := Bounds(v.total, v.offset, v.limit)
		if a != v.start || b != v.end {
			t.Fatalf("%+v: %d %d", v, a, b)
		}
	}
	p := Slice([]int{1, 2, 3}, 1, 1)
	if len(p) != 1 || p[0] != 2 {
		t.Fatal(p)
	}
}
