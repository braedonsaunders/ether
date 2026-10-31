package plex

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// Watch progress is the client's /:/timeline time. A transcode session
// status, including one whose encoder is minutes ahead of playback, does
// not write state_watch.
func TestWatchProgressFollowsClientTimelineNotEncoder(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	watch := state.New(store.OpenTest(t), state.Options{})
	f.srv.d.State = watch

	const client = 1426 * time.Second // 23:46
	rec := f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&state=playing&time=1426000&duration=5424620")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline: %d %s", rec.Code, rec.Body.String())
	}
	st, err := watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if err != nil {
		t.Fatal(err)
	}
	if st.ViewOffset != client {
		t.Fatalf("view offset = %v, want client timeline %v", st.ViewOffset, client)
	}

	// The encoder head from the stalled Abzurdah session (47:54). Nothing
	// in session status may persist it.
	if rec = f.get(t, "/transcode/sessions"); rec.Code != http.StatusOK {
		t.Fatalf("transcode sessions: %d", rec.Code)
	}
	st, err = watch.Get(context.Background(), ownerUser.ID, fightClub.Key)
	if err != nil {
		t.Fatal(err)
	}
	if st.ViewOffset != client {
		t.Fatalf("view offset = %v after encoder status, want %v", st.ViewOffset, client)
	}
}
