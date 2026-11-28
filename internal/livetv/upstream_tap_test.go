package livetv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A provider sold by the connection sees one connection per playing
// channel: the sniff, the probe, ffmpeg and every other viewer of that
// channel share it, the account's limit refuses a further channel, and
// stopping the last viewer closes the connection before Stop returns.
func TestOneProviderConnectionPerChannel(t *testing.T) {
	var open, total atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticated(r) {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		total.Add(1)
		open.Add(1)
		defer open.Add(-1)
		w.Header().Set("Content-Type", "video/mp2t")
		for i := 0; ; i++ {
			if _, err := fmt.Fprintf(w, "PKT%05d", i); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}))
	defer up.Close()
	tc := &fakeTranscoder{}
	s := testService(t, Options{Transcoder: tc, HTTPClient: up.Client()})
	ctx := context.Background()
	playlist := "#EXTM3U\n#EXTINF:-1 tvg-id=\"one\",One\n" + up.URL + "/live/1.ts\n#EXTINF:-1 tvg-id=\"two\",Two\n" + up.URL + "/live/2.ts\n"
	if _, err := s.ImportSource(ctx, Source{ID: "provider", Name: "Provider", Auth: relayAuth(), Playlist: playlist}); err != nil {
		t.Fatal(err)
	}
	// The account allows one connection (an Xtream panel reports it).
	e, err := s.sourceEntry(ctx, "provider")
	if err != nil {
		t.Fatal(err)
	}
	e.limit = 1
	s.setSourceEntry("provider", e)
	cs, _ := s.Channels(ctx)
	one, two := cs[0], cs[1]

	first, err := s.Tune(ctx, &core.User{ID: 1}, one.ID, core.ClientCaps{})
	if err != nil {
		t.Fatal(err)
	}
	// ffprobe and ffmpeg both read the relay input from the first byte.
	input := tc.requests[0].InputURL
	for range 2 {
		res, err := http.Get(input)
		if err != nil {
			t.Fatal(err)
		}
		head := make([]byte, 8)
		_, err = io.ReadFull(res.Body, head)
		res.Body.Close()
		if err != nil || string(head) != "PKT00000" {
			t.Fatalf("reader started at %q: %v", head, err)
		}
	}
	// Leave one response unread while another subscriber reads from the
	// same tap. Reader backpressure is isolated from the producer.
	stalled, err := http.Get(input)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Body.Close()
	fast, err := http.Get(input)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_, err = io.ReadFull(fast.Body, buf)
	fast.Body.Close()
	if err != nil {
		t.Fatal("unread subscriber blocked another reader", err)
	}
	stalled.Body.Close()
	second, err := s.Tune(ctx, &core.User{ID: 2}, one.ID, core.ClientCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if total.Load() != 1 {
		t.Fatalf("one channel, two viewers, %d provider connections", total.Load())
	}
	if _, err := s.Tune(ctx, &core.User{ID: 3}, two.ID, core.ClientCaps{}); !errors.Is(err, ErrProviderBusy) || !errors.Is(err, core.ErrStreamLimit) {
		t.Fatalf("second channel on a one-connection account: %v", err)
	}
	s.Stop(ctx, first.ID, &core.User{ID: 1})
	if s.inputRelay.taps.open("provider") != 1 {
		t.Fatal("the remaining viewer lost the shared connection")
	}
	s.Stop(ctx, second.ID, &core.User{ID: 2})
	if s.inputRelay.taps.open("provider") != 0 {
		t.Fatal("the connection outlived its last viewer")
	}
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if open.Load() != 0 {
		t.Fatal("provider connection still open after the last viewer stopped")
	}
	third, err := s.Tune(ctx, &core.User{ID: 3}, two.ID, core.ClientCaps{})
	if err != nil {
		t.Fatalf("freed connection not reusable: %v", err)
	}
	s.Stop(ctx, third.ID, &core.User{ID: 3})
	if total.Load() != 2 {
		t.Fatalf("provider connections = %d", total.Load())
	}
}

// New provider streams are spaced by the gap and capped per hour.
func TestTapPacesNewProviderStreams(t *testing.T) {
	var r tapRegistry
	now := time.Unix(1_000_000, 0)
	for i, want := range []time.Duration{0, 2 * time.Second, 4 * time.Second} {
		if wait, err := r.reserveOpen("p", 2*time.Second, 3, now); err != nil || wait != want {
			t.Fatalf("open %d: wait %v err %v", i, wait, err)
		}
	}
	if _, err := r.reserveOpen("p", 2*time.Second, 3, now.Add(time.Minute)); !errors.Is(err, ErrProviderPaced) {
		t.Fatalf("fourth open in the hour: %v", err)
	}
	if wait, err := r.reserveOpen("p", 2*time.Second, 3, now.Add(time.Hour+5*time.Second)); err != nil || wait != 0 {
		t.Fatalf("after the hour: %v %v", wait, err)
	}
	if wait, err := r.reserveOpen("other", 0, 0, now); err != nil || wait != 0 {
		t.Fatalf("unpaced source: %v %v", wait, err)
	}
}

// A slow background sourceEntry fill started before ImportSource/DeleteSource
// or setSourceEntry must not overwrite a fresher authCache entry once its DB
// query returns (PostgreSQL multi-connection pool race).
func TestAuthCacheRejectsStaleConcurrentFill(t *testing.T) {
	s := testService(t, Options{})
	ctx := context.Background()
	if _, err := s.ImportSource(ctx, Source{
		ID:       "provider",
		Name:     "Provider",
		Auth:     relayAuth(),
		Playlist: "#EXTM3U\n#EXTINF:-1 tvg-id=\"one\",One\nhttp://example.invalid/live/1.ts\n",
	}); err != nil {
		t.Fatal(err)
	}
	s.forgetSourceAuth("provider")

	// Simulate a background loop reading authCache.gen before a mutation, then
	// losing the race to setSourceEntry while its DB read is in flight.
	s.authCache.mu.Lock()
	staleGen := s.authCache.gen
	s.authCache.mu.Unlock()

	fresh := authEntry{limit: 1, at: s.o.Now()}
	s.setSourceEntry("provider", fresh)

	stale := authEntry{limit: 0, at: s.o.Now()}
	s.authCache.mu.Lock()
	if s.authCache.gen == staleGen {
		s.authCache.m["provider"] = stale
	}
	s.authCache.mu.Unlock()

	if got := s.sourceConnectionLimit("provider"); got != 1 {
		t.Fatalf("stale background fill overwrote connection limit: got %d, want 1", got)
	}
}

func TestTapIgnoresIdleCallbackFromPreviousReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tap := &upstreamTap{closedDone: make(chan struct{})}
		tap.cond = sync.NewCond(&tap.mu)
		defer tap.close(nil)
		tap.attach()
		tap.detach()
		previous := tap.idle

		tap.attach()
		tap.detach()
		// Simulate the old callback already queued as the tap lost and
		// regained its last reader. It cannot expire this idle period.
		previous.Reset(0)
		synctest.Wait()
		select {
		case <-tap.closedDone:
			t.Fatal("old idle callback closed the tap during a new idle period")
		default:
		}

		time.Sleep(tapIdle)
		synctest.Wait()
		select {
		case <-tap.closedDone:
		default:
			t.Fatal("tap did not close after its current idle period")
		}
	})
}
