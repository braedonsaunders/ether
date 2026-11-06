package plex

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A continuous queue must respect the season shown to the viewer, for
// both initial playback and Add to Up Next. Later seasons are not loaded.
func TestContinuousQueueStopsAtPresentedSeason(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	season2 := bbS1
	season2.Key, season2.Season = core.SeasonKey(breakingBad.Key.TMDB(), 2), 2
	ep := bbE1
	ep.Key, ep.Season = core.EpisodeKey(breakingBad.Key.TMDB(), 2, 1), 2
	f.catalog.titles[season2.Key], f.catalog.titles[ep.Key] = season2, ep
	f.catalog.children[breakingBad.Key] = append(f.catalog.children[breakingBad.Key], season2.Key)
	f.catalog.children[season2.Key] = []core.RatingKey{ep.Key}
	for _, tc := range []struct {
		start core.RatingKey
		want  int
	}{{bbE1.Key, 2}, {bbE2.Key, 1}, {ep.Key, 1}} {
		keys, err := f.srv.continuousKeys(context.Background(), &Request{User: &ownerUser}, "/library/metadata/"+tc.start.String(), true)
		if err != nil || len(keys) != tc.want || keys[0] != tc.start {
			t.Fatalf("start %s: %v %v", tc.start, keys, err)
		}
	}
	create := f.request(t, "POST", "/playQueues?type=video&uri="+url.QueryEscape("/library/metadata/"+fightClub.Key.String()), map[string]string{"Accept": "application/json"})
	id := decodeJSON(t, create)["playQueueID"]
	add := f.request(t, "PUT", fmt.Sprintf("/playQueues/%.0f?continuous=1&uri=", id)+url.QueryEscape("/library/metadata/"+bbE1.Key.String()), map[string]string{"Accept": "application/json"})
	if add.Code != 200 || decodeJSON(t, add)["playQueueTotalCount"] != float64(3) {
		t.Fatalf("add: %d %s", add.Code, add.Body.String())
	}
}
