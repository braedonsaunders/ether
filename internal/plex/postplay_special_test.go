package plex

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestShowQueueAndUnverifiedNextTimeline(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	show := core.ShowKey(81723)
	f.catalog.titles[show] = core.Title{Key: show, Type: core.TypeShow, Title: "Show"}
	var episode core.RatingKey
	for season := 0; season <= 4; season++ {
		sk := core.SeasonKey(81723, season)
		ep := core.EpisodeKey(81723, season, 1)
		f.catalog.titles[sk] = core.Title{Key: sk, Type: core.TypeSeason, ShowKey: show, Season: season}
		f.catalog.titles[ep] = core.Title{Key: ep, Type: core.TypeEpisode, ShowKey: show, Season: season, Episode: 1, Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
		f.catalog.children[show] = append(f.catalog.children[show], sk)
		f.catalog.children[sk] = []core.RatingKey{ep}
		if season == 4 {
			episode = ep
		}
	}
	special := core.EpisodeKey(81723, 0, 1)
	uri := "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + show.String()
	rec := f.request(t, http.MethodPost, "/playQueues?type=video&uri="+urlEsc(uri)+"&key="+episode.String(), map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("queue: %d %s", rec.Code, rec.Body.String())
	}
	queue := decodeJSON(t, rec)
	if queue["playQueueTotalCount"] != float64(4) {
		t.Fatalf("show queue includes specials: %v", queue)
	}
	uri = "server://0123456789abcdef0123456789abcdef01234567/com.plexapp.plugins.library/library/metadata/" + core.SeasonKey(81723, 0).String()
	rec = f.request(t, http.MethodPost, "/playQueues?type=video&uri="+urlEsc(uri), map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["playQueueTotalCount"] != float64(1) {
		t.Fatalf("special queue: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+episode.String()+"&session=t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("decision: %d %s", rec.Code, rec.Body.String())
	}
	f.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=stopped&time=3300000&duration=3361000", episode))
	rec = f.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=playing&time=3300000&duration=540000", special))
	if rec.Code != http.StatusBadRequest || recordedAt(f.state.progress, special, 3300*time.Second) {
		t.Fatalf("stale playhead recorded: %+v", f.state.progress)
	}
	// A real position for the same item still counts, with or without a
	// source on this node (cluster hand-off, downloads).
	f.get(t, fmt.Sprintf("/:/timeline?ratingKey=%s&state=playing&time=500000&duration=540000", special))
	if !recordedAt(f.state.progress, special, 500*time.Second) {
		t.Fatalf("real progress was not recorded: %+v", f.state.progress)
	}
	f.get(t, "/:/scrobble?key="+special.String()+"&identifier=com.plexapp.plugins.library")
	if len(f.state.scrobbed) != 1 || f.state.scrobbed[0] != special {
		t.Fatalf("explicit scrobble: %+v", f.state.scrobbed)
	}
}

// recordedAt reports whether progress for key was written at offset.
func recordedAt(calls []progressCall, key core.RatingKey, offset time.Duration) bool {
	for _, c := range calls {
		if c.key == key && c.offset == offset {
			return true
		}
	}
	return false
}
