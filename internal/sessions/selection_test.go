package sessions

import "testing"

func TestSelectionsAreScopedByViewerAndItem(t *testing.T) {
	s := NewSelections[int]()
	s.Update(1, 9, func(v *int) { *v = 3 })
	s.Update(2, 9, func(v *int) { *v = 5 })
	s.Update(1, 10, func(v *int) { *v = 7 })
	if s.Get(1, 9) != 3 || s.Get(2, 9) != 5 || s.Get(1, 10) != 7 || s.Get(2, 10) != 0 {
		t.Fatal("selections crossed viewer or item")
	}
}
