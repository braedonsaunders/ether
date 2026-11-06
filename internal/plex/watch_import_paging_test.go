package plex

import (
	"fmt"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestWatchImportPagingIsViewerScopedAndTerminates(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key, ViewCount: 4, UserRating: 7.5})
	for _, token := range []string{ownerToken, friendToken} {
		seen := map[string]bool{}
		for start := 0; start < 4; start++ {
			mc := decodeJSON(t, f.get(t, "/library/sections/1/all?type=1", "Accept", "application/json", "X-Plex-Token", token, "X-Plex-Container-Start", fmt.Sprint(start), "X-Plex-Container-Size", "1"))
			rows, _ := mc["Metadata"].([]any)
			if mc["offset"] != float64(start) || len(rows) > 1 {
				t.Fatalf("bad page %d: %+v", start, mc)
			}
			if len(rows) == 0 {
				if len(seen) != 2 {
					t.Fatalf("early empty page: %v", seen)
				}
				break
			}
			row := rows[0].(map[string]any)
			key := row["ratingKey"].(string)
			if seen[key] {
				t.Fatal("repeated item in import pages")
			}
			seen[key] = true
			if token == ownerToken && key == fightClub.Key.String() {
				if row["viewCount"] != float64(4) || row["userRating"] != 7.5 {
					t.Fatalf("watch/rating omitted: %+v", row)
				}
			} else if _, ok := row["userRating"]; ok {
				t.Fatalf("viewer rating leaked: %+v", row)
			}
		}
	}
	if len(f.state.progress) != 0 {
		t.Fatal("watch import read wrote progress")
	}
}
