package plex

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/plex/model"
)

func TestRecentlyAddedExplicitScopes(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, tc := range []struct {
		query       string
		movie, show bool
	}{
		{"", true, true},
		{"?type=1", true, false},
		{"?type=2", false, true},
		{"?type=1&sectionID=1", true, false},
		{"?type=2&sectionID=1", false, false},
		{"?type=1&contentDirectoryID=1,2", true, false},
		{"?type=1&contentDirectoryID=2", false, false},
		{"?type=2&contentDirectoryID=1", false, false},
		{"?type=1&pinnedContentDirectoryID=1", true, false},
		{"?type=1&pinnedContentDirectoryID=2", false, false},
		{"?type=1&contentDirectoryID=1&pinnedContentDirectoryID=2", false, false},
		{"?contentDirectoryID=1", false, false},
		{"?pinnedContentDirectoryID=2", false, false},
		{"?sectionID=1", false, false},
		{"?type=3", false, false},
		{"?type=4", false, false},
		{"?type=15", false, false},
		{"?type=18", false, false},
		{"?type=999", false, false},
		{"?type=1,4", false, false},
		{"?type=2,4&contentDirectoryID=2", false, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := f.request(t, http.MethodGet, "/hubs/home/recentlyAdded"+tc.query, map[string]string{"Accept": "application/json"})
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var body struct{ MediaContainer model.MediaContainer }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			var movie, show bool
			for _, item := range body.MediaContainer.Metadata {
				switch item.Type {
				case "movie":
					movie = true
				case "show":
					show = true
				default:
					t.Fatalf("unexpected item type %q", item.Type)
				}
			}
			if movie != tc.movie || show != tc.show {
				t.Fatalf("movie=%v show=%v; want %v %v", movie, show, tc.movie, tc.show)
			}
		})
	}
}
