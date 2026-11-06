package livetv

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A dense channel with blocked availability cannot hold server construction or
// another channel's guide. Cold reads retry explicitly; preparation is bounded.
func TestGuideStartupAdmission(t *testing.T) {
	ctx := t.Context()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for i, id := range []string{"dense-a", "ordinary", "dense-b"} {
		c := testChannel()
		c.ID, c.Number, c.Epoch = id, string(rune('1'+i)), now
		c.Items = []Item{{Key: core.MovieKey(int64(i + 1)), Title: id, DurationMS: 16000}}
		if id == "ordinary" {
			c.Items[0].DurationMS = 3600000
		}
		b, _ := json.Marshal(c)
		if _, err := db.ExecContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?)`, id, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	defer release()
	availability := &startupAvailability{gate: gate}
	type result struct {
		s   *Service
		err error
	}
	ready := make(chan result, 1)
	go func() {
		s, err := New(Options{DB: db, Now: func() time.Time { return now }, Availability: availability, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		ready <- result{s, err}
	}()
	var s *Service
	select {
	case r := <-ready:
		s, err = r.s, r.err
	case <-time.After(time.Second):
		release()
		r := <-ready
		if r.s != nil {
			r.s.Close()
		}
		t.Fatal("service construction waited for guide preparation")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer release()
	h := s.Handler()
	read := func(id string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/guide?channels="+id, nil))
		return w
	}
	for deadline := time.Now().Add(2 * time.Second); ; {
		if w := read("ordinary"); w.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("blocked channel prevented the ordinary channel from warming")
		}
		time.Sleep(time.Millisecond)
	}
	for _, id := range []string{"dense-a", "dense-b"} {
		w := read(id)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("cold %s returned %d, Retry-After %q", id, w.Code, w.Header().Get("Retry-After"))
		}
	}
	permitted, err := s.AllowedChannelsForIDs(ctx, &core.User{IsOwner: true}, []string{"ordinary"})
	if err != nil || len(permitted) != 1 || permitted[0].ID != "ordinary" {
		t.Fatalf("selected-channel permission check waited for a cold neighbour: %v", err)
	}
	if availability.maximum.Load() > 2 {
		t.Fatalf("unbounded preparation: %d jobs", availability.maximum.Load())
	}
	release()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if read("dense-a").Code == http.StatusOK && read("dense-b").Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("released channels did not become available")
		}
		time.Sleep(time.Millisecond)
	}
}

type startupAvailability struct {
	gate            <-chan struct{}
	active, maximum atomic.Int64
}

func (a *startupAvailability) Available(ctx context.Context, key core.RatingKey) (bool, bool) {
	if key != core.MovieKey(2) {
		n := a.active.Add(1)
		defer a.active.Add(-1)
		for old := a.maximum.Load(); old < n && !a.maximum.CompareAndSwap(old, n); old = a.maximum.Load() {
		}
		select {
		case <-a.gate:
		case <-ctx.Done():
		}
	}
	return true, true
}
