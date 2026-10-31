//go:build plexparity

package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestParitySettleWarmWaitsUntilTheProbeFinishes(t *testing.T) {
	user := &core.User{ID: 9}
	caps := core.ClientCaps{ClientID: "adult", Product: "Plex for Android (TV)", Protocol: "plex"}
	key := core.MovieKey(10004)
	done := make(chan struct{})
	m := &Manager{
		now:    time.Now,
		warm:   map[identity]*warmEntry{},
		closed: true, // Prewarm refuses, leaving the planted in-flight entry to be observed.
	}
	m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}] = &warmEntry{
		done:    done,
		expires: time.Now().Add(time.Minute),
	}
	errc := make(chan error, 1)
	go func() {
		errc <- m.ParitySettleWarm(context.Background(), user, key, caps)
	}()
	select {
	case err := <-errc:
		t.Fatalf("returned before the probe finished: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(done)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the probe")
	}
}

func TestParitySettleWarmReturnsWhenNoProbeIsRunning(t *testing.T) {
	m := &Manager{now: time.Now, warm: map[identity]*warmEntry{}, closed: true}
	if err := m.ParitySettleWarm(context.Background(), &core.User{ID: 9}, core.MovieKey(10004), core.ClientCaps{ClientID: "adult", Product: "Plex"}); err != nil {
		t.Fatal(err)
	}
}
