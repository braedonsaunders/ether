package state

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestOriginSurvivesDeferredProgress(t *testing.T) {
	db := store.OpenTest(t)
	available := store.NewAvailability(nil)
	s := New(db, Options{Availability: available})
	origin := Origin{Surface: "jellyfin", SessionID: "play", ClientID: "tv"}
	events := make(chan Event, 4)
	s.Subscribe(func(ev Event) { events <- ev })
	available.Fail(syscall.ECONNREFUSED)
	if err := s.Progress(WithOrigin(context.Background(), origin), 2, core.MovieKey(9), time.Minute, time.Hour, "paused"); err != nil {
		t.Fatal(err)
	}
	available.OK()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if ev.Origin != origin {
			t.Fatalf("origin %+v", ev.Origin)
		}
	case <-time.After(time.Second):
		t.Fatal("deferred event missing")
	}
}
