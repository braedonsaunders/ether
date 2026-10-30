package experience

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/libpath"
)

func TestVirtualPathMovieCollisionsMatchLibpath(t *testing.T) {
	first := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Duplicate", Year: 2020}
	second := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Duplicate", Year: 2020}
	catalog := movieCatalog([]core.Title{second, first})
	viewer := &core.Viewer{ID: 7}
	device := core.Device{ID: "living-room"}
	a, err := VirtualPath(context.Background(), catalog, viewer, device, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := VirtualPath(context.Background(), catalog, viewer, device, second)
	if err != nil {
		t.Fatal(err)
	}
	names := libpath.Disambiguate([]libpath.NamedKey{{Name: libpath.MovieName(first), Key: first.Key}, {Name: libpath.MovieName(second), Key: second.Key}})
	wantA := path.Join("Movies", names[first.Key], names[first.Key]+".mkv")
	wantB := path.Join("Movies", names[second.Key], names[second.Key]+".mkv")
	if a != wantA || b != wantB || a == b || !strings.Contains(a, first.Key.String()) || !strings.Contains(b, second.Key.String()) {
		t.Fatalf("paths: %q %q", a, b)
	}
	batch, err := VirtualPaths(context.Background(), catalog, viewer, device, []core.RatingKey{second.Key, first.Key, second.Key})
	if err != nil || batch[first.Key] != a || batch[second.Key] != b || len(batch) != 2 {
		t.Fatalf("batch: %+v %v", batch, err)
	}
	if _, err = VirtualPath(context.Background(), catalog, viewer, device, core.Title{Key: core.MovieKey(9), Type: core.TypeMovie, Title: "Missing"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing title: %v", err)
	}
}

func TestVirtualPathsShortPagesShareOneScan(t *testing.T) {
	first := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Film"}
	second := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Film"}
	foreign := core.Title{Key: core.ShowKey(8), Type: core.TypeShow, Title: "Film"}
	var scans int
	var sawDevice string
	catalog := VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			return []VirtualPathView{{ID: "movies", Type: core.TypeMovie}, {ID: "other", Type: core.TypeShow}}, nil
		},
		LibraryItems: func(_ context.Context, _ *core.Viewer, device core.Device, id string, offset, limit int) (VirtualPathPage, error) {
			if id != "movies" || limit != virtualPathLibraryPage {
				t.Fatalf("query: %s %d", id, limit)
			}
			sawDevice = device.ID
			scans++
			if offset == 0 {
				return VirtualPathPage{Items: []core.Title{first, foreign}, Total: 3}, nil
			}
			return VirtualPathPage{Items: []core.Title{second}, Total: 3}, nil
		},
	}
	got, err := VirtualPaths(context.Background(), catalog, &core.Viewer{ID: 7}, core.Device{ID: "tv"}, []core.RatingKey{first.Key, second.Key, core.MovieKey(999)})
	if err != nil || scans != 2 || sawDevice != "tv" || len(got) != 2 || got[first.Key] == got[second.Key] || !strings.Contains(got[first.Key], first.Key.String()) {
		t.Fatalf("batch: %+v scans=%d device=%s err=%v", got, scans, sawDevice, err)
	}
	if _, ok := got[foreign.Key]; ok {
		t.Fatalf("show included in movie paths: %+v", got)
	}
}

func TestVirtualPathsBoundsRejectBeforeScan(t *testing.T) {
	var calls int
	catalog := VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			calls++
			return nil, nil
		},
		LibraryItems: func(context.Context, *core.Viewer, core.Device, string, int, int) (VirtualPathPage, error) {
			t.Fatal("library scanned without a section")
			return VirtualPathPage{}, nil
		},
	}
	wide := make([]core.RatingKey, virtualPathMaxInput+1)
	for n := range wide {
		wide[n] = core.MovieKey(1)
	}
	if _, err := VirtualPaths(context.Background(), catalog, &core.Viewer{ID: 7}, core.Device{}, wide); !errors.Is(err, core.ErrNotAvailable) || calls != 0 {
		t.Fatalf("wide input: %v calls=%d", err, calls)
	}
	distinct := make([]core.RatingKey, virtualPathMaxDistinct+1)
	for n := range distinct {
		distinct[n] = core.MovieKey(int64(n + 1))
	}
	if _, err := VirtualPaths(context.Background(), catalog, &core.Viewer{ID: 7}, core.Device{}, distinct); !errors.Is(err, core.ErrNotAvailable) || calls != 0 {
		t.Fatalf("distinct input: %v calls=%d", err, calls)
	}
	repeated := make([]core.RatingKey, virtualPathMaxInput)
	for n := range repeated {
		repeated[n] = core.MovieKey(1)
	}
	got, err := VirtualPaths(context.Background(), catalog, &core.Viewer{ID: 7}, core.Device{}, repeated)
	if err != nil || calls != 1 || len(got) != 0 {
		t.Fatalf("repeated bound: %+v calls=%d err=%v", got, calls, err)
	}
	empty, err := VirtualPaths(context.Background(), VirtualPathCatalog{}, &core.Viewer{ID: 7}, core.Device{}, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
}

func TestVirtualPathsMalformedPagesAndDeniedSectionLeaveNoTree(t *testing.T) {
	viewer := &core.Viewer{ID: 7}
	movie := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Film"}
	show := core.Title{Key: core.ShowKey(4), Type: core.TypeShow, Title: "Show"}
	malformed := VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			return []VirtualPathView{{ID: "movies", Type: core.TypeMovie}}, nil
		},
		LibraryItems: func(context.Context, *core.Viewer, core.Device, string, int, int) (VirtualPathPage, error) {
			return VirtualPathPage{Total: 2}, nil
		},
	}
	if got, err := VirtualPaths(context.Background(), malformed, viewer, core.Device{}, []core.RatingKey{movie.Key}); got != nil || !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("empty library page: %+v %v", got, err)
	}
	denied := VirtualPathCatalog{Views: func(_ context.Context, v *core.Viewer) ([]VirtualPathView, error) {
		if v != viewer {
			t.Fatal("viewer")
		}
		return nil, core.ErrUnauthorized
	}}
	if got, err := VirtualPaths(context.Background(), denied, viewer, core.Device{}, []core.RatingKey{movie.Key}); got != nil || !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("denied section: %+v %v", got, err)
	}
	if _, err := VirtualPath(context.Background(), denied, viewer, core.Device{}, movie); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("denied title walk: %v", err)
	}
	var libraryCalls int
	partial := VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			return []VirtualPathView{{ID: "movies", Type: core.TypeMovie}, {ID: "tv", Type: core.TypeShow}}, nil
		},
		LibraryItems: func(_ context.Context, _ *core.Viewer, _ core.Device, id string, _, _ int) (VirtualPathPage, error) {
			libraryCalls++
			if id == "tv" {
				return VirtualPathPage{}, errors.New("section offline")
			}
			return VirtualPathPage{Items: []core.Title{movie}, Total: 1}, nil
		},
	}
	episode := core.EpisodeKey(show.Key.TMDB(), 1, 1)
	if got, err := VirtualPaths(context.Background(), partial, viewer, core.Device{}, []core.RatingKey{movie.Key, episode}); got != nil || err == nil || libraryCalls != 2 {
		t.Fatalf("partial tree: %+v calls=%d err=%v", got, libraryCalls, err)
	}
	childrenErr := VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			return []VirtualPathView{{ID: "tv", Type: core.TypeShow}}, nil
		},
		LibraryItems: func(context.Context, *core.Viewer, core.Device, string, int, int) (VirtualPathPage, error) {
			return VirtualPathPage{Items: []core.Title{show}, Total: 1}, nil
		},
		Children: func(context.Context, *core.Viewer, core.Device, core.RatingKey, int, int) (VirtualPathPage, error) {
			return VirtualPathPage{}, core.ErrUnauthorized
		},
	}
	if got, err := VirtualPaths(context.Background(), childrenErr, viewer, core.Device{}, []core.RatingKey{core.SeasonKey(show.Key.TMDB(), 1)}); got != nil || !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("denied children: %+v %v", got, err)
	}
}

func TestVirtualPathChildrenContinueThenRejectTruncation(t *testing.T) {
	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow, Title: "Show"}
	var limits []int
	catalog := VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			return []VirtualPathView{{ID: "tv", Type: core.TypeShow}}, nil
		},
		LibraryItems: func(context.Context, *core.Viewer, core.Device, string, int, int) (VirtualPathPage, error) {
			return VirtualPathPage{Items: []core.Title{show}, Total: 1}, nil
		},
		Children: func(_ context.Context, _ *core.Viewer, _ core.Device, _ core.RatingKey, offset, limit int) (VirtualPathPage, error) {
			limits = append(limits, limit)
			if offset == 2 {
				return VirtualPathPage{Total: 3}, nil
			}
			return VirtualPathPage{Items: []core.Title{{Key: core.SeasonKey(1, offset+1), Type: core.TypeSeason, Season: offset + 1}}, Total: 3}, nil
		},
	}
	got, err := VirtualPaths(context.Background(), catalog, &core.Viewer{ID: 7}, core.Device{}, []core.RatingKey{core.EpisodeKey(1, 1, 1)})
	if got != nil || !errors.Is(err, core.ErrNotAvailable) || len(limits) != 3 || limits[0] != virtualPathChildrenPage {
		t.Fatalf("truncated children: %+v %v limits=%v", got, err, limits)
	}
	bounded := VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			return []VirtualPathView{{ID: "movies", Type: core.TypeMovie}}, nil
		},
		LibraryItems: func(context.Context, *core.Viewer, core.Device, string, int, int) (VirtualPathPage, error) {
			return VirtualPathPage{Items: make([]core.Title, virtualPathScanLimit), Total: virtualPathScanLimit + 1}, nil
		},
	}
	if _, err := VirtualPaths(context.Background(), bounded, &core.Viewer{ID: 7}, core.Device{}, []core.RatingKey{core.MovieKey(1)}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("scan bound: %v", err)
	}
	if _, err := VirtualPath(context.Background(), VirtualPathCatalog{}, &core.Viewer{ID: 7}, core.Device{}, core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("nil port: %v", err)
	}
}

func TestVirtualPathEpisodeUsesPresentedSeasonAndSafeShow(t *testing.T) {
	show := core.Title{Key: core.ShowKey(10), Type: core.TypeShow, Title: "A/B"}
	other := core.Title{Key: core.ShowKey(11), Type: core.TypeShow, Title: "A/B"}
	season := core.Title{Key: core.SeasonKey(10, 9000), Type: core.TypeSeason, ShowKey: show.Key, Season: 9000}
	episode := core.Title{Key: core.EpisodeKey(10, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key, ShowTitle: "Show", Season: 1, Episode: 2}
	extra := core.Title{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "Not a season"}
	catalog := VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			return []VirtualPathView{{ID: "tv", Type: core.TypeShow}}, nil
		},
		LibraryItems: func(_ context.Context, _ *core.Viewer, _ core.Device, _ string, offset, _ int) (VirtualPathPage, error) {
			if offset == 0 {
				return VirtualPathPage{Items: []core.Title{other, extra}, Total: 3}, nil
			}
			return VirtualPathPage{Items: []core.Title{show}, Total: 3}, nil
		},
		Children: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey, _ int, _ int) (VirtualPathPage, error) {
			if key == show.Key {
				return VirtualPathPage{Items: []core.Title{extra, season}, Total: 2}, nil
			}
			return VirtualPathPage{Items: []core.Title{episode}, Total: 1}, nil
		},
	}
	viewer := &core.Viewer{ID: 7}
	got, err := VirtualPath(context.Background(), catalog, viewer, core.Device{}, episode)
	showName := fmt.Sprintf("A B [%d]", show.Key)
	want := path.Join("TV", showName, "Season 9000", "Show - S01E02.mkv")
	if err != nil || got != want {
		t.Fatalf("path: %q want %q err %v", got, want, err)
	}
	batch, err := VirtualPaths(context.Background(), catalog, viewer, core.Device{}, []core.RatingKey{show.Key, season.Key, episode.Key})
	if err != nil || batch[show.Key] != path.Join("TV", showName) || batch[season.Key] != path.Join("TV", showName, "Season 9000") || batch[episode.Key] != want {
		t.Fatalf("batch: %+v %v", batch, err)
	}
	if _, err = VirtualPath(context.Background(), catalog, viewer, core.Device{}, core.Title{Type: core.TypeEpisode}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing show key: %v", err)
	}
	if _, err = VirtualPath(context.Background(), catalog, viewer, core.Device{}, core.Title{Key: show.Key, Type: core.MediaType("collection")}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unsupported type: %v", err)
	}
}

func movieCatalog(items []core.Title) VirtualPathCatalog {
	return VirtualPathCatalog{
		Views: func(context.Context, *core.Viewer) ([]VirtualPathView, error) {
			return []VirtualPathView{{ID: "movies", Type: core.TypeMovie}}, nil
		},
		LibraryItems: func(_ context.Context, _ *core.Viewer, _ core.Device, _ string, offset, _ int) (VirtualPathPage, error) {
			if offset > 0 {
				return VirtualPathPage{Total: len(items)}, nil
			}
			return VirtualPathPage{Items: append([]core.Title(nil), items...), Total: len(items)}, nil
		},
	}
}
