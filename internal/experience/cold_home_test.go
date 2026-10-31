package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestColdHomeRowsSkipHiddenLibrariesAndCapThePage(t *testing.T) {
	viewer := &core.User{ID: 3, AllowedSections: []int{1}}
	libraries := []Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}, {ID: 2, Name: "TV Shows", Type: core.TypeShow}}
	var limits []int
	rows, err := ColdHomeRows(context.Background(), viewer, libraries, Visibility{}, 500, []core.RatingKey{core.MovieKey(9)}, func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		limits = append(limits, q.Limit)
		if len(q.Filter.ExcludeKeys) != 1 || q.Filter.ExcludeKeys[0] != core.MovieKey(9) {
			t.Fatalf("exclude %v", q.Filter.ExcludeKeys)
		}
		item := core.Title{Key: core.MovieKey(1), Type: q.Filter.Type, Title: "One"}
		return core.Page{Items: []core.Title{item}}, nil
	})
	if err != nil || len(rows) != 1 || rows[0].ID != "library.recent.1" || rows[0].Purpose != frontend.RowRecentlyAdded || len(limits) != 1 || limits[0] == 0 {
		t.Fatalf("rows %+v limits %v err %v", rows, limits, err)
	}
}

func TestMaySetViewerRatingOwnerOnly(t *testing.T) {
	owner := &core.User{ID: 1, IsOwner: true}
	if !MaySetViewerRating(&core.Viewer{ID: 1}, owner) {
		t.Fatal("owner")
	}
	if MaySetViewerRating(&core.Viewer{ID: 1, Delegated: true}, owner) || MaySetViewerRating(&core.Viewer{ID: 2}, &core.User{ID: 2}) || MaySetViewerRating(nil, owner) {
		t.Fatal("denied actor was allowed")
	}
}

func TestLibrarySectionMapsEpisodesToShows(t *testing.T) {
	libraries := []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}
	id, ok := LibrarySection(libraries, core.TypeEpisode)
	if !ok || id != 2 {
		t.Fatalf("episode library %d %v", id, ok)
	}
	v := BindVisibility(nil, nil, nil, nil, libraries)
	if !v.TitleAllowed(&core.User{AllowedSections: []int{2}}, &core.Title{Type: core.TypeShow}) {
		t.Fatal("show library hidden")
	}
	if v.TitleAllowed(&core.User{AllowedSections: []int{2}}, &core.Title{Type: core.TypeMovie}) {
		t.Fatal("movie library visible")
	}
}
