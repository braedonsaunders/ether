package plex

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Filtering precedes pagination; type projections preserve the first member's
// parent order and collapse siblings, while plain membership retains duplicates.
func TestPlaylistQueryProjectionAndPaging(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	a, _ := f.pls.Create(ctx, ownerUser.ID, "Zulu", []core.RatingKey{fightClub.Key})
	b, _ := f.pls.Create(ctx, ownerUser.ID, "Alpha", []core.RatingKey{alien.Key, bbE1.Key, bbE1.Key})
	rows := func(path string) map[string]any {
		t.Helper()
		rec := f.get(t, path, jsonAccept...)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return decodeJSON(t, rec)
	}
	mc := rows("/playlists?title=Alpha&X-Plex-Container-Start=0&X-Plex-Container-Size=1&includeMeta=1")
	if mc["totalSize"] != float64(1) || mc["Metadata"].([]any)[0].(map[string]any)["ratingKey"] != b.Key.String() {
		t.Fatalf("filter/page: %v", mc)
	}
	md := mc["Metadata"].([]any)[0].(map[string]any)
	if md["viewCount"] != float64(1) || md["lastViewedAt"] != md["addedAt"] {
		t.Fatalf("initial state: %v", md)
	}
	direct := rows("/playlists/" + b.Key.String())["Metadata"].([]any)[0].(map[string]any)
	if _, ok := direct["viewCount"]; ok {
		t.Fatalf("direct read state: %v", direct)
	}
	mc = rows("/playlists?sort=mediaCount:desc")
	if mc["Metadata"].([]any)[0].(map[string]any)["ratingKey"] != b.Key.String() {
		t.Fatalf("count sort: %v", mc)
	}
	for _, typ := range []string{"2", "3"} {
		mc = rows("/playlists/" + b.Key.String() + "/items?type=" + typ + "&includeMeta=1")
		if mc["size"] != float64(1) {
			t.Fatalf("projection %s: %v", typ, mc)
		}
		if _, ok := mc["Metadata"].([]any)[0].(map[string]any)["playlistItemID"]; ok {
			t.Fatalf("projected parent has item ID: %v", mc)
		}
	}
	mc = rows("/playlists/" + b.Key.String() + "/items?type=4")
	if mc["size"] != float64(2) {
		t.Fatalf("duplicates: %v", mc)
	}
	for path, status := range map[string]int{"/playlists?sort=leafCount": 500, "/playlists?type=abc": 500, "/playlists/" + a.Key.String() + "/items?type=1.5": 400, "/playlists/" + a.Key.String() + "/items?limit=abc": 500} {
		rec := f.get(t, path)
		if rec.Code != status {
			t.Fatalf("%s: %d want %d", path, rec.Code, status)
		}
	}
	mc = rows("/playlists/all?playlistType=video,audio&sectionID=1&type=15&includeMeta=1&sort=mediaCount:desc")
	tab := mc["Meta"].(map[string]any)["Type"].([]any)[0].(map[string]any)
	if tab["active"] != false || !strings.Contains(tab["key"].(string), "playlistType=video%2Caudio&sectionID=1&type=15") {
		t.Fatalf("meta links: %v", tab)
	}
	for _, entry := range tab["Sort"].([]any) {
		if entry.(map[string]any)["active"] != false {
			t.Fatalf("all path active sort: %v", entry)
		}
	}
	empty, _ := f.pls.Create(ctx, ownerUser.ID, "Empty", nil)
	rec := f.request(t, http.MethodGet, "/playlists/"+empty.Key.String(), map[string]string{"Accept": "application/json"})
	md = decodeJSON(t, rec)["Metadata"].([]any)[0].(map[string]any)
	if _, ok := md["composite"]; ok {
		t.Fatalf("empty composite: %v", md)
	}
}

func TestPlaylistKnownDurationBeyondSmallPage(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	title := fightClub
	title.Runtime = 1800689 * time.Millisecond
	f.srv.titles.put(&title)
	keys := make([]core.RatingKey, 201)
	for i := range keys {
		keys[i] = title.Key
	}
	if got := f.srv.playlistDuration(keys); got != 201*1800000 {
		t.Fatalf("duration: %d", got)
	}
}
