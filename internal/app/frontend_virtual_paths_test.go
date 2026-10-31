package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestVirtualPathsBatchSharesLibraryScanAndOmitsHidden(t *testing.T) {
	first := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Film"}
	second := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Film"}
	var scans int
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *core.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: core.TypeMovie}}, nil
		},
		LibraryItemsFunc: func(context.Context, *core.Viewer, core.Device, string, frontend.ItemQuery) (core.Page, error) {
			scans++
			return core.Page{Items: []core.Title{first, second}}, nil
		},
	}
	s := surfaceVirtualPaths{experience: experience}
	got, err := s.Paths(context.Background(), &core.Viewer{ID: 7}, core.Device{}, []core.RatingKey{first.Key, second.Key, first.Key, core.MovieKey(999)})
	if err != nil || scans != 1 || len(got) != 2 || got[first.Key] == got[second.Key] {
		t.Fatalf("batch: %+v scans=%d err=%v", got, scans, err)
	}
	keys := make([]core.RatingKey, 51)
	for n := range keys {
		keys[n] = core.MovieKey(int64(n + 1))
	}
	if _, err := s.Paths(context.Background(), &core.Viewer{ID: 7}, core.Device{}, keys); err == nil {
		t.Fatal("unbounded batch accepted")
	}
}

func TestVirtualPathsScanShortPagesForCollisions(t *testing.T) {
	first := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Film"}
	second := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Film"}
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *core.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: core.TypeMovie}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *core.Viewer, _ core.Device, _ string, q frontend.ItemQuery) (core.Page, error) {
			return core.Page{Items: []core.Title{[]core.Title{first, second}[q.Offset]}, Total: 2}, nil
		},
	}
	got, err := (surfaceVirtualPaths{experience: experience}).Paths(context.Background(), &core.Viewer{ID: 7}, core.Device{}, []core.RatingKey{first.Key, second.Key})
	if err != nil || len(got) != 2 || !strings.Contains(got[first.Key], first.Key.String()) || !strings.Contains(got[second.Key], second.Key.String()) {
		t.Fatalf("paths: %v, %v", got, err)
	}
}

func TestVirtualPathsChildrenContinueShortPagesAndRejectTruncation(t *testing.T) {
	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow, Title: "Show"}
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *core.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "tv", Type: core.TypeShow}}, nil
		},
		LibraryItemsFunc: func(context.Context, *core.Viewer, core.Device, string, frontend.ItemQuery) (core.Page, error) {
			return core.Page{Items: []core.Title{show}, Total: 1}, nil
		},
		ChildrenFunc: func(_ context.Context, _ *core.Viewer, _ core.Device, _ core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
			if q.Offset == 2 {
				return core.Page{Total: 3}, nil
			}
			return core.Page{Items: []core.Title{{Key: core.SeasonKey(1, q.Offset+1), Type: core.TypeSeason, Season: q.Offset + 1}}, Total: 3}, nil
		},
	}
	_, err := (surfaceVirtualPaths{experience: experience}).Paths(context.Background(), &core.Viewer{ID: 7}, core.Device{}, []core.RatingKey{core.EpisodeKey(1, 1, 1)})
	if !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("truncated children: %v", err)
	}
}

func TestVirtualPathsMalformedLibraryAndDeniedSectionFailClosed(t *testing.T) {
	viewer := &core.Viewer{ID: 7}
	malformed := frontendtest.Experience{
		ViewsFunc: func(context.Context, *core.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: core.TypeMovie}}, nil
		},
		LibraryItemsFunc: func(context.Context, *core.Viewer, core.Device, string, frontend.ItemQuery) (core.Page, error) {
			return core.Page{Total: 2}, nil
		},
	}
	if got, err := (surfaceVirtualPaths{experience: malformed}).Paths(context.Background(), viewer, core.Device{}, []core.RatingKey{core.MovieKey(1)}); got != nil || !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("malformed page: %+v %v", got, err)
	}
	denied := frontendtest.Experience{ViewsFunc: func(context.Context, *core.Viewer) ([]frontend.View, error) {
		return nil, core.ErrUnauthorized
	}}
	if got, err := (surfaceVirtualPaths{experience: denied}).Paths(context.Background(), viewer, core.Device{}, []core.RatingKey{core.MovieKey(1)}); got != nil || !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("denied section: %+v %v", got, err)
	}
}

func TestVirtualPathsPageDoesNotWaitForTheFullIndex(t *testing.T) {
	movie := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Film"}
	show := core.Title{Key: core.ShowKey(2), Type: core.TypeShow, Title: "Show"}
	release := make(chan struct{})
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *core.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: core.TypeMovie}, {ID: "tv", Type: core.TypeShow}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *core.Viewer, _ core.Device, id string, _ frontend.ItemQuery) (core.Page, error) {
			if id == "tv" {
				return core.Page{Items: []core.Title{show}, Total: 1}, nil
			}
			return core.Page{Items: []core.Title{movie}, Total: 1}, nil
		},
		ChildrenFunc: func(context.Context, *core.Viewer, core.Device, core.RatingKey, frontend.ItemQuery) (core.Page, error) {
			<-release
			return core.Page{Items: []core.Title{}, Total: 0}, nil
		},
	}
	s := surfaceVirtualPaths{experience: experience, index: &pathIndex{}, stamp: func(v *core.Viewer) string {
		if v == nil {
			return ""
		}
		return "viewer"
	}, enabled: func() bool { return true }}
	t.Cleanup(func() { close(release) })
	done := make(chan struct {
		paths map[core.RatingKey]string
		err   error
	}, 1)
	go func() {
		got, err := s.Paths(context.Background(), &core.Viewer{ID: 7}, core.Device{}, []core.RatingKey{movie.Key})
		done <- struct {
			paths map[core.RatingKey]string
			err   error
		}{got, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.paths[movie.Key] == "" {
			t.Fatalf("page waited for the show walk: %v %v", got.paths, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("page blocked on the full index")
	}
}

func TestVirtualPathBaseFailsClosedWhenWebDAVDisabled(t *testing.T) {
	enabled := false
	s := surfaceVirtualPaths{enabled: func() bool { return enabled }, base: func(context.Context, *http.Request) (string, error) { return "https://ether.example/webdav", nil }}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := s.Base(context.Background(), req); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatal(err)
	}
	enabled = true
	if got, err := s.Base(context.Background(), req); err != nil || got != "https://ether.example/webdav" {
		t.Fatal(got, err)
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
