package plex

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	watchstate "github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestOnDeckUsesRecentWatchedAnchorAndSkipsEpisodeZero(t *testing.T) {
	eps := []core.Title{}
	for _, n := range []int{1, 2, 3, 4} {
		eps = append(eps, core.Title{Key: core.EpisodeKey(44, 1, n), Type: core.TypeEpisode, Season: 1, Episode: n})
	}
	eps = append(eps, core.Title{Key: core.EpisodeKey(44, 2, 0), Type: core.TypeEpisode, Season: 2, Episode: 0})
	eps = append(eps, core.Title{Key: core.EpisodeKey(44, 2, 1), Type: core.TypeEpisode, Season: 2, Episode: 1})
	states := map[core.RatingKey]core.WatchStatus{eps[3].Key: {ViewCount: 1, LastViewedAt: fixedNow.Add(-time.Hour)}, eps[0].Key: {ViewCount: 1, LastViewedAt: fixedNow}}
	selectNext := func() *core.Title { return experience.ShowOnDeck(eps, states, func(*core.Title) bool { return false }) }
	if got := selectNext(); got == nil || got.Key != eps[1].Key {
		t.Fatalf("recent anchor: %+v", got)
	}
	delete(states, eps[0].Key)
	if got := selectNext(); got == nil || got.Key != eps[5].Key {
		t.Fatalf("episode zero selected: %+v", got)
	}
	states[eps[1].Key] = core.WatchStatus{ViewOffset: time.Minute, LastViewedAt: fixedNow}
	states[eps[2].Key] = core.WatchStatus{ViewOffset: time.Minute, LastViewedAt: fixedNow.Add(time.Minute)}
	if got := selectNext(); got == nil || got.Key != eps[2].Key {
		t.Fatalf("recent resume: %+v", got)
	}
}

func TestPostplayWatchStateEligibility(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.Recommender = nil
	path := "/hubs/metadata/" + bbE1.Key.String() + "/postplay"
	read := func() map[string]any { return decodeJSON(t, f.get(t, path, jsonAccept...)) }
	if mc := read(); mc["size"] != float64(0) {
		t.Fatalf("pristine show got postplay: %v", mc)
	}
	f.state.set(ownerUser.ID, ws(bbE1.Key, 100000, 0))
	if mc := read(); mc["size"] != float64(0) {
		t.Fatalf("self resume got postplay: %v", mc)
	}
	f.state.set(ownerUser.ID, ws(bbE1.Key, 0, 1))
	mc := read()
	hubs, _ := mc["Hub"].([]any)
	if len(hubs) != 1 {
		t.Fatalf("watched show missing next-up: %v", mc)
	}
	hub := hubs[0].(map[string]any)
	if hub["upNext"] != true {
		t.Fatalf("missing next-up flag: %v", hub)
	}
	if hub["Metadata"].([]any)[0].(map[string]any)["ratingKey"] != bbE2.Key.String() {
		t.Fatal(hub)
	}
	if mc["allowSync"] != false {
		t.Fatalf("postplay sync attribute: %v", mc)
	}
}

func TestHomeResumeMembershipAndDirectoryGrammar(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, ws(bbE1.Key, 100000, 0))
	mc := decodeJSON(t, f.get(t, "/hubs?identifier=home.continue,home.ondeck", jsonAccept...))
	for _, row := range mc["Hub"].([]any) {
		if row.(map[string]any)["hubIdentifier"] == "home.ondeck" {
			t.Fatal("duplicated resume shelf", mc)
		}
	}
	mc = decodeJSON(t, f.get(t, "/hubs?identifier=home.continue,home.ondeck&excludeContinueWatching=001", jsonAccept...))
	if rows, _ := mc["Hub"].([]any); len(rows) > 0 {
		t.Fatal("padded exclusion ignored", mc)
	}
	for _, directory := range []string{"999", "", "999,garbage"} {
		rec := f.get(t, "/hubs/continueWatching?contentDirectoryID="+directory, jsonAccept...)
		if rec.Code != http.StatusOK {
			t.Fatalf("directory %q: %d %s", directory, rec.Code, rec.Body)
		}
	}
	for _, directory := range []string{"garbage", "0", "-1"} {
		if rec := f.get(t, "/hubs/continueWatching?contentDirectoryID="+directory); rec.Code != 404 {
			t.Fatalf("directory %q: %d", directory, rec.Code)
		}
	}
	if rec := f.get(t, "/hubs/sections/999/continueWatching/items"); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	st, err := f.state.Get(context.Background(), ownerUser.ID, bbE1.Key)
	if err != nil || st.ViewOffset != 100000*time.Millisecond {
		t.Fatal(st, err)
	}
}

func TestContinueRemovalWithoutPersonalServicePreservesResume(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.Discovery = nil
	st := watchstate.New(db, watchstate.Options{Now: func() time.Time { return fixedNow }})
	defer st.Close(ctx)
	f.srv.d.State = st
	if err := st.Progress(ctx, ownerUser.ID, bbE1.Key, 600000*time.Millisecond, time.Hour, "playing"); err != nil {
		t.Fatal(err)
	}
	path := "/actions/removeFromContinueWatching?ratingKey=" + bbE1.Key.String()
	// Use the advertised action rather than a state mutation shortcut.
	rec := f.request(t, "PUT", path, nil)
	if rec.Code == 404 {
		rec = f.request(t, "PUT", "/library/metadata/"+bbE1.Key.String()+"/removeFromContinueWatching?ratingKey="+bbE1.Key.String(), nil)
	}
	if rec.Code != 200 {
		t.Fatalf("remove action: %d %s", rec.Code, rec.Body)
	}
	status, err := st.Get(ctx, ownerUser.ID, bbE1.Key)
	if err != nil || status.ViewOffset != 600000*time.Millisecond {
		t.Fatal(status, err)
	}
	hidden, err := st.ContinueHidden(ctx, ownerUser.ID, []core.RatingKey{core.ShowKey(bbE1.Key.TMDB())})
	if err != nil || !hidden[core.ShowKey(bbE1.Key.TMDB())] {
		t.Fatal(hidden, err)
	}
	mc := decodeJSON(t, f.get(t, "/hubs/continueWatching/items", jsonAccept...))
	if mc["size"] != float64(0) {
		t.Fatal("hidden home row", mc)
	}
	mc = decodeJSON(t, f.get(t, "/hubs/sections/2/continueWatching/items", jsonAccept...))
	if mc["size"] != float64(1) {
		t.Fatal("TV section must retain hidden row", mc)
	}
}
