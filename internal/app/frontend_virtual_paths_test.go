package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestVirtualPathsRespectViewerPolicyAndDisambiguate(t *testing.T) {
	first := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Duplicate", Year: 2020}
	second := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Duplicate", Year: 2020}
	viewer := &core.Viewer{ID: 7}
	experience := frontendtest.Experience{
		ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
			if r.Viewer.ID != 7 {
				return frontend.ItemDetail{}, core.ErrNotFound
			}
			if r.Key == first.Key {
				return frontend.ItemDetail{Title: first}, nil
			}
			if r.Key == second.Key {
				return frontend.ItemDetail{Title: second}, nil
			}
			return frontend.ItemDetail{}, core.ErrNotFound
		},
		ViewsFunc: func(_ context.Context, v *core.Viewer) ([]frontend.View, error) {
			if v.ID != 7 {
				return nil, core.ErrUnauthorized
			}
			return []frontend.View{{ID: "movies", Type: core.TypeMovie}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, v *core.Viewer, _ core.Device, _ string, _ frontend.ItemQuery) (core.Page, error) {
			if v.ID != 7 {
				return core.Page{}, core.ErrUnauthorized
			}
			return core.Page{Items: []core.Title{first, second}}, nil
		},
	}
	s := surfaceVirtualPaths{experience: experience}
	a, err := s.Path(context.Background(), viewer, core.Device{}, first.Key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Path(context.Background(), viewer, core.Device{}, second.Key)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasSuffix(a, ".mkv") || !strings.Contains(a, first.Key.String()) || !strings.Contains(b, second.Key.String()) {
		t.Fatalf("colliding paths: %q %q", a, b)
	}
	if _, err = s.Path(context.Background(), &core.Viewer{ID: 8}, core.Device{}, first.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden item: %v", err)
	}
}

func TestVirtualEpisodePathUsesPresentedSeason(t *testing.T) {
	show := core.Title{Key: core.ShowKey(10), Type: core.TypeShow, Title: "Show"}
	season := core.Title{Key: core.SeasonKey(10, 9000), Type: core.TypeSeason, ShowKey: show.Key, Season: 9000}
	episode := core.Title{Key: core.EpisodeKey(10, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key, ShowTitle: "Show", Season: 1, Episode: 2}
	experience := frontendtest.Experience{
		ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: episode}, nil
		},
		ViewsFunc: func(context.Context, *core.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "tv", Type: core.TypeShow}}, nil
		},
		LibraryItemsFunc: func(context.Context, *core.Viewer, core.Device, string, frontend.ItemQuery) (core.Page, error) {
			return core.Page{Items: []core.Title{show}}, nil
		},
		ChildrenFunc: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey, _ frontend.ItemQuery) (core.Page, error) {
			if key == show.Key {
				return core.Page{Items: []core.Title{season}}, nil
			}
			return core.Page{Items: []core.Title{episode}}, nil
		},
	}
	got, err := (surfaceVirtualPaths{experience: experience}).Path(context.Background(), &core.Viewer{ID: 7}, core.Device{}, episode.Key)
	if err != nil || got != "TV/Show/Season 9000/Show - S01E02.mkv" {
		t.Fatalf("path: %q %v", got, err)
	}
}
