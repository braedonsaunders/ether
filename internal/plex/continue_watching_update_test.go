package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestMarkOneWatchedRetainsOtherContinueWatchingItems(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	var keys []core.RatingKey
	for n := range 4 {
		title := fightClub
		title.Key = core.MovieKey(9000 + int64(n))
		title.Title = fmt.Sprintf("Resume test %d", n)
		keys = append(keys, title.Key)
		f.catalog.titles[title.Key] = title
		f.state.set(ownerUser.ID, ws(title.Key, 600000+int64(n)*60000, 0))
	}
	count := func(path string, want int) {
		t.Helper()
		mc := decodeJSON(t, f.get(t, path, jsonAccept...))
		if hubs, ok := mc["Hub"].([]any); ok {
			var found bool
			for _, h := range hubs {
				hub := h.(map[string]any)
				if hub["title"] == "Continue Watching" {
					mc = hub
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s omitted Continue Watching", path)
			}
		}
		items, _ := mc["Metadata"].([]any)
		if len(items) != want {
			t.Fatalf("%s has %d resume items, want %d", path, len(items), want)
		}
	}
	paths := []string{"/hubs/continueWatching", "/hubs/home/continueWatching", "/hubs/sections/1/continueWatching/items", "/hubs/sections/1", "/hubs?identifier=home.continue,home.ondeck"}
	for _, p := range paths {
		count(p, 4)
	}
	if rec := f.get(t, "/:/scrobble?key="+keys[0].String()+"&identifier=com.plexapp.plugins.library"); rec.Code != http.StatusOK {
		t.Fatalf("scrobble: HTTP %d", rec.Code)
	}
	for _, p := range paths {
		count(p, 3)
	}
	for n, k := range keys[1:] {
		st, err := f.state.Get(context.Background(), ownerUser.ID, k)
		if err != nil || st.ViewOffset.Milliseconds() != 600000+int64(n+1)*60000 {
			t.Fatalf("unrelated resume state changed: %+v %v", st, err)
		}
	}
}

// Plex Web repopulates a stale shelf with a paginated request after scrobble.
// Its metadata-list reducer defaults an absent totalSize to zero and hides it.
func TestContinueWatchingRefreshIncludesPagination(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	var keys []core.RatingKey
	for n := range 4 {
		title := fightClub
		title.Key = core.MovieKey(9100 + int64(n))
		title.Title = fmt.Sprintf("Resume pagination %d", n)
		keys = append(keys, title.Key)
		f.catalog.titles[title.Key] = title
		f.state.set(ownerUser.ID, ws(title.Key, 600000, 0))
	}
	if rec := f.get(t, "/:/scrobble?key="+keys[0].String()+"&identifier=com.plexapp.plugins.library"); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	for _, path := range []string{"/hubs/sections/1/continueWatching/items", "/hubs/home/continueWatching", "/hubs/continueWatching/items"} {
		for _, page := range []struct{ start, size, want int }{{0, 4, 3}, {1, 1, 1}, {3, 2, 0}, {0, 0, 0}} {
			mc := decodeJSON(t, f.get(t, path, "Accept", "application/json", "X-Plex-Container-Start", fmt.Sprint(page.start), "X-Plex-Container-Size", fmt.Sprint(page.size)))
			if mc["totalSize"] != float64(3) || mc["offset"] != float64(page.start) || mc["size"] != float64(page.want) {
				t.Fatalf("%s page %+v: size=%v totalSize=%v offset=%v", path, page, mc["size"], mc["totalSize"], mc["offset"])
			}
			rows, _ := mc["Metadata"].([]any)
			if len(rows) != page.want {
				t.Fatalf("page items=%d want %d", len(rows), page.want)
			}
			for _, row := range rows {
				if row.(map[string]any)["ratingKey"] == keys[0].String() {
					t.Fatal("watched movie remains")
				}
			}
		}
	}
}

func TestPlaybackWatchRefreshUsesLibraryTimelineAndViewerScope(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	owner, _ := f.srv.notifier.subscribe([]string{"timeline"}, &ownerUser)
	friend, _ := f.srv.notifier.subscribe(nil, &friendUser)
	defer f.srv.notifier.unsubscribe(owner)
	defer f.srv.notifier.unsubscribe(friend)
	f.srv.WatchStateChanged(ownerUser.ID, bbE1.Key)
	select {
	case msg := <-owner.ch:
		var body struct {
			NotificationContainer struct {
				Type          string
				TimelineEntry []struct {
					ItemID    string `json:"itemID"`
					SectionID string `json:"sectionID"`
					Type      int    `json:"type"`
					State     int    `json:"state"`
				}
			}
		}
		if err := json.Unmarshal(msg.data, &body); err != nil {
			t.Fatal(err)
		}
		n := body.NotificationContainer
		if n.Type != "timeline" || len(n.TimelineEntry) != 1 {
			t.Fatalf("bad refresh %s", msg.data)
		}
		e := n.TimelineEntry[0]
		if e.ItemID != bbE1.Key.String() || e.SectionID != "2" || e.Type != 4 || e.State != 5 {
			t.Fatalf("wrong episode refresh %s", msg.data)
		}
	default:
		t.Fatal("playback watch change sent no library refresh")
	}
	select {
	case msg := <-friend.ch:
		t.Fatalf("other viewer received private watch update %s", msg.data)
	default:
	}
}
