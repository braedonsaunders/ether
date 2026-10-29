package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type fakeVibe struct {
	items []core.Title
	ok    bool
	err   error
}

func (f fakeVibe) VibeCollection(context.Context, *core.User, string) (*core.Collection, []core.Title, bool, error) {
	return &core.Collection{Title: "vibe"}, f.items, f.ok, f.err
}
func TestVibeSearch(t *testing.T) {
	s := VibeSearch{Source: fakeVibe{ok: true, items: []core.Title{{Key: 1}, {Key: 2, Adult: true}}}}
	c, items, ok, err := s.Find(context.Background(), &core.User{}, "vibe")
	if err != nil || !ok || c.Title != "vibe" || len(items) != 1 || items[0].Key != 1 {
		t.Fatalf("unexpected results: %+v %+v %v %v", c, items, ok, err)
	}
	s.Source = fakeVibe{err: errors.New("unavailable")}
	_, _, _, err = s.Find(context.Background(), &core.User{}, "vibe")
	if err == nil {
		t.Fatal("source error not propagated")
	}
	s.Source = nil
	_, _, ok, err = s.Find(context.Background(), &core.User{}, "vibe")
	if ok || err != nil {
		t.Fatal("missing source must not claim query")
	}
}
