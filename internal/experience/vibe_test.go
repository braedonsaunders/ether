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

func TestVibeSearchFiltersInheritedPolicyAndAvailability(t *testing.T) {
	ctx := context.Background()
	s := VibeSearch{
		Source: fakeVibe{ok: true, items: []core.Title{{Key: 1, ShowKey: 5}, {Key: 2}, {Key: 3}}},
		Visibility: Visibility{
			GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &core.Title{Adult: true}, nil },
			FilterAvailable: func(_ context.Context, ts []core.Title) []core.Title {
				if ts[0].Key == 2 {
					return nil
				}
				return ts
			},
		},
	}
	_, got, ok, err := s.Find(ctx, &core.User{}, "query")
	if err != nil || !ok || len(got) != 1 || got[0].Key != 3 {
		t.Fatalf("unexpected results: %+v %v %v", got, ok, err)
	}
}

func TestVibeTrustedLegacyOutcome(t *testing.T) {
	failure := errors.New("vibe unavailable")
	title := core.Title{Key: 1, Adult: true}
	for _, tc := range []struct {
		name string
		ok   bool
		err  error
	}{{"not matched", false, nil}, {"failed", true, failure}} {
		t.Run(tc.name, func(t *testing.T) {
			s := VibeSearch{Source: fakeVibe{items: []core.Title{title}, ok: tc.ok, err: tc.err}, Unrestricted: func(*core.User) bool { return true }}
			collection, items, ok, err := s.Find(context.Background(), &core.User{}, "query")
			if collection == nil || len(items) != 1 || items[0].Key != title.Key || ok != tc.ok || !errors.Is(err, tc.err) {
				t.Fatalf("outcome=%+v %+v %v %v", collection, items, ok, err)
			}
		})
	}
}

func TestVibeUntrustedFailureDoesNotReturnSourcePayload(t *testing.T) {
	failure := errors.New("unavailable")
	for _, tc := range []struct {
		ok  bool
		err error
	}{{false, nil}, {true, failure}} {
		s := VibeSearch{Source: fakeVibe{items: []core.Title{{Key: 1}}, ok: tc.ok, err: tc.err}}
		c, items, ok, err := s.Find(context.Background(), &core.User{}, "query")
		if c != nil || items != nil || ok != tc.ok || !errors.Is(err, tc.err) {
			t.Fatalf("untrusted outcome=%+v %+v %v %v", c, items, ok, err)
		}
	}
}
