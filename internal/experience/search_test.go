package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSearchDiscoveryAndFallback(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{AllowedSections: []int{1}}
	called := false
	s := Search{
		Visibility: Visibility{SectionFor: func(typ core.MediaType) (int, bool) {
			if typ == core.TypeMovie {
				return 1, true
			}
			return 2, true
		}},
		Natural: func(context.Context, *core.User, string, int) ([]core.Title, bool) {
			return []core.Title{{Key: 1, Type: core.TypeMovie}, {Key: 2, Type: core.TypeMovie, Adult: true}, {Key: 3, Type: core.TypeShow}}, true
		},
		Catalog: func(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
			called = true
			return nil, errors.New("catalog failure")
		},
	}
	got, err := s.Find(ctx, viewer, "test", []core.MediaType{core.TypeMovie}, 10)
	if err != nil || called || len(got) != 1 || got[0].Key != 1 {
		t.Fatalf("discovery results: %+v, %v, catalog called %v", got, err, called)
	}
	s.Natural = nil
	_, err = s.Find(ctx, viewer, "test", []core.MediaType{core.TypeMovie}, 10)
	if !called || err == nil {
		t.Fatal("catalogue errors must propagate")
	}
	called = false
	got, err = s.Find(ctx, viewer, " ", []core.MediaType{core.TypeMovie}, 10)
	if err != nil || got != nil || called {
		t.Fatal("blank search must not query catalogue")
	}
}

func TestSearchCatalogueEnforcesViewerPolicy(t *testing.T) {
	s := Search{
		Visibility: Visibility{SectionFor: func(t core.MediaType) (int, bool) {
			if t == core.TypeMovie {
				return 1, true
			}
			return 2, true
		}},
		Catalog: func(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
			return []core.Title{{Key: 1, Type: core.TypeMovie}, {Key: 2, Type: core.TypeShow}, {Key: 3, Type: core.TypeMovie, Adult: true}}, nil
		},
	}
	got, err := s.Find(context.Background(), &core.User{AllowedSections: []int{1}}, "query", []core.MediaType{core.TypeMovie}, 10)
	if err != nil || len(got) != 1 || got[0].Key != 1 {
		t.Fatalf("unexpected results: %+v, %v", got, err)
	}
}

func TestSearchFiltersBeforeLimitAndBackfills(t *testing.T) {
	ctx := context.Background()
	parent := core.ShowKey(10)
	episode := core.Title{Key: 11, ShowKey: parent, Type: core.TypeEpisode}
	items := []core.Title{episode, {Key: 12, Type: core.TypeMovie}, {Key: 13, Type: core.TypeMovie}}
	calls := 0
	s := Search{
		Visibility: Visibility{
			GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &core.Title{Adult: true}, nil },
			FilterAvailable: func(_ context.Context, titles []core.Title) []core.Title {
				if titles[0].Key == 12 {
					return nil
				}
				return titles
			},
		},
		Catalog: func(_ context.Context, _ string, _ []core.MediaType, limit int) ([]core.Title, error) {
			calls++
			return items[:min(limit, len(items))], nil
		},
	}
	got, err := s.Find(ctx, &core.User{}, "query", []core.MediaType{core.TypeMovie, core.TypeEpisode}, 1)
	if err != nil || len(got) != 1 || got[0].Key != 13 || calls != 3 {
		t.Fatalf("backfill: %+v, %v, calls %d", got, err, calls)
	}
}
