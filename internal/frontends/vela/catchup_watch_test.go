package vela

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type catchUpWatchLive struct {
	*frontendtest.CatchUpLive
	ids    []string
	saved  []time.Duration
	states []string
}

func (f *catchUpWatchLive) CatchUpProgress(_ context.Context, _ *frontend.Viewer, id string, offset, _ time.Duration, state string) error {
	f.ids, f.saved, f.states = append(f.ids, id), append(f.saved, offset), append(f.states, state)
	return nil
}

// A played-back airing's position is saved as its watch state, spaced out
// while it plays; start-over and time-range sessions have no library airing.
// Vela starts every airing from the beginning, so it never reports a stop
// that would clear a resume point saved from another app.
func TestCatchUpPositionSaved(t *testing.T) {
	live := &catchUpWatchLive{CatchUpLive: frontendtest.NewCatchUpLive()}
	i := &instance{host: frontend.Host{LiveTV: live}}
	p := live.Item.Programme
	info := frontend.CatchUpInfo{ID: "archive", ChannelID: p.ChannelID, ProgrammeID: p.ID, Start: p.Start, End: p.End}
	s := &playSession{archive: &catchUpPlayback{info: info}}
	viewer := &frontend.Viewer{ID: 1}
	now := time.Unix(1700010000, 0)
	save := func(state string, at time.Time, position time.Duration) bool {
		t.Helper()
		got, ok := s.catchUpToSave(state, at)
		if ok {
			i.saveCatchUp(t.Context(), viewer, got, position, state)
		}
		return ok
	}
	if !save("playing", now, time.Minute) {
		t.Fatal("first playing sample not saved")
	}
	if save("playing", now.Add(5*time.Second), 2*time.Minute) {
		t.Fatal("playing samples not spaced out")
	}
	if save("buffering", now.Add(20*time.Second), 3*time.Minute) {
		t.Fatal("buffering sample saved")
	}
	if !save("paused", now.Add(6*time.Second), 4*time.Minute) {
		t.Fatal("pause transition not saved")
	}
	if save("paused", now.Add(7*time.Second), 4*time.Minute) {
		t.Fatal("repeated paused sample was saved")
	}
	if !save("playing", now.Add(17*time.Second), 5*time.Minute) {
		t.Fatal("due playing sample not saved")
	}
	if !save("stopped", now.Add(18*time.Second), 6*time.Minute) || !save("ended", now.Add(19*time.Second), 58*time.Minute) {
		t.Fatal("stop or end not saved")
	}
	want := []string{"playing", "paused", "playing", "paused", "paused"}
	if len(live.states) != len(want) {
		t.Fatalf("saved states %v, want %v", live.states, want)
	}
	for n := range want {
		if live.states[n] != want[n] || live.ids[n] != frontend.CatchUpEpisodeID(p.ID) {
			t.Fatalf("save %d: %s %s, want %s", n, live.ids[n], live.states[n], want[n])
		}
	}
	if live.saved[len(live.saved)-1] != 58*time.Minute {
		t.Fatalf("end position: %v", live.saved)
	}
	for _, other := range []frontend.CatchUpInfo{
		{ChannelID: p.ChannelID, ProgrammeID: p.ID, StartOver: true},
		{ChannelID: p.ChannelID, Start: p.Start, End: p.End},
	} {
		s := &playSession{archive: &catchUpPlayback{info: other}}
		if _, ok := s.catchUpToSave("paused", now); ok {
			t.Fatalf("saved a session with no library airing: %+v", other)
		}
	}
	if _, ok := (&playSession{}).catchUpToSave("paused", now); ok {
		t.Fatal("saved a session that is not catch-up")
	}
}
