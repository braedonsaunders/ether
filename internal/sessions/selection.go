package sessions

import "sync"

// Selections stores per-viewer track choices. The adapter owns the value
// shape and translates wire stream identifiers before updating a choice.
type Selections[T any] struct {
	mu     sync.Mutex
	values map[[2]int64]T
}

func NewSelections[T any]() *Selections[T] { return &Selections[T]{values: map[[2]int64]T{}} }
func (s *Selections[T]) Get(viewer, item int64) T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[[2]int64{viewer, item}]
}
func (s *Selections[T]) Update(viewer, item int64, fn func(*T)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.values) > 100_000 {
		s.values = map[[2]int64]T{}
	}
	key := [2]int64{viewer, item}
	value := s.values[key]
	fn(&value)
	s.values[key] = value
}
