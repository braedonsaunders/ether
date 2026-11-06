package plex

import (
	"net/http"
	"testing"
)

// Plex Web builds the show Play URI from OnDeck.Metadata.key. Unlike a
// MediaContainer's Metadata list, PMS returns this one episode as an object.
// An array looks like an episode without a key to Plex Web, which then posts
// only server://<id>/com.plexapp.plugins.library and gets a queue error.
func TestShowPlayUsesOnDeckEpisodeObject(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/library/metadata/"+breakingBad.Key.String()+"?includeOnDeck=1", jsonAccept...)
	if rec.Code != http.StatusOK {
		t.Fatalf("show detail: %d", rec.Code)
	}
	show := decodeJSON(t, rec)["Metadata"].([]any)[0].(map[string]any)
	onDeck, ok := show["OnDeck"].(map[string]any)
	if !ok {
		t.Fatal("show has no OnDeck")
	}
	ep, ok := onDeck["Metadata"].(map[string]any)
	if !ok {
		t.Fatalf("OnDeck.Metadata must be one episode object, got %T", onDeck["Metadata"])
	}
	key, _ := ep["key"].(string)
	if key != "/library/metadata/"+bbE1.Key.String() || ep["type"] != "episode" {
		t.Fatalf("wrong next episode: key=%q type=%v", key, ep["type"])
	}
	uri := "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library" + key
	rec = f.request(t, http.MethodPost, "/playQueues?type=video&continuous=1&uri="+urlEsc(uri), map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("show Play queue: %d %s", rec.Code, rec.Body.String())
	}
	queue := decodeJSON(t, rec)
	if queue["playQueueSelectedMetadataItemID"] != bbE1.Key.String() || queue["playQueueTotalCount"] != float64(2) {
		t.Fatalf("wrong show queue: %v", queue)
	}
}
