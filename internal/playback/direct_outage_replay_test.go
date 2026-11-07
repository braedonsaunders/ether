package playback

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Direct-play clients retain the original file coordinates. An unproven
// replacement must require a fresh decision, and no alternate must end the
// session, even if the client never sends another retry.
func TestBoundedDirectOutageRecovery(t *testing.T) {
	for _, alternate := range []bool{false, true} {
		t.Run(map[bool]string{false: "no alternate", true: "other release"}[alternate], func(t *testing.T) {
			a := mkv("A", 200000, 100*time.Second)
			ids := []string{"A"}
			rels := []*fakeRelease{a}
			if alternate {
				ids = append(ids, "B")
				rels = append(rels, mkv("B", 200000, 100*time.Second))
			}
			h := newHarness(t, rels, ids)
			ps := h.prepare(owner, movieKey, "outage")
			s := h.m.lookup(ps.ID)
			g := s.current()
			<-g.indexReady
			g.src = retryDirectSource{g.src}
			a.setFail(120000, 130000, core.ErrSourceUnavailable)
			h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionDirectPlay})
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Range", "bytes=120000-120999")
			first := httptest.NewRecorder()
			h.m.ServeRange(first, req, ps.ID)
			if first.Code != http.StatusServiceUnavailable {
				t.Fatalf("initial failure status=%d", first.Code)
			}
			g.mu.Lock()
			if !g.retryPending {
				g.mu.Unlock()
				t.Fatal("unavailable source did not start recovery timer")
			}
			// Invalidate the production 25s timer and exercise its same path
			// using a short clock for this offline invariant check.
			g.retryPending = false
			g.retryEpoch++
			g.mu.Unlock()
			h.m.limitDirectRetryAfter(s, g, 30*time.Second, 20*time.Millisecond)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				state, ok := h.m.Session(ps.ID)
				if !alternate && !ok {
					return
				}
				if alternate && ok && state.RequiresRemux {
					w := httptest.NewRecorder()
					h.m.ServeRange(w, httptest.NewRequest("GET", "/", nil), ps.ID)
					if w.Code != http.StatusConflict {
						t.Fatalf("next request status=%d", w.Code)
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("outage did not terminate or hand off")
		})
	}
}

// A hung backend need not return an error before recovery begins.
type stalledRetrySource struct{ core.MediaSource }

func (stalledRetrySource) RetryDirectPlay() bool { return true }
func (stalledRetrySource) ReadAtContext(ctx context.Context, _ []byte, _ int64) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestDirectStallReadBudgetEndsSession(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a}, []string{"A"})
	ps := h.prepare(owner, movieKey, "stalled")
	s := h.m.lookup(ps.ID)
	g := s.current()
	<-g.indexReady
	g.src = stalledRetrySource{g.src}
	ctx := context.WithValue(context.Background(), directReadBudgetKey{}, 20*time.Millisecond)
	err := h.m.streamClient(ctx, io.Discard, s, 120000, 4096)
	if !errors.Is(err, errEndResponse) {
		t.Fatalf("stalled read: %v", err)
	}
	// One budget expiry reads like a temporarily stalled CDN edge, not a
	// dead release: the bounded stall grace keeps the original release once.
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("first stalled read ended the session")
	}
	if err := h.m.streamClient(ctx, io.Discard, s, 120000, 4096); !errors.Is(err, errEndResponse) {
		t.Fatalf("second stalled read: %v", err)
	}
	if _, ok := h.m.Session(ps.ID); ok {
		t.Fatal("stalled session remains active")
	}
}

type pausedRetrySource struct{ core.MediaSource }

func (pausedRetrySource) RetryDirectPlay() bool { return false }

func TestDirectProviderPauseWithoutAlternateEndsSession(t *testing.T) {
	a := mkv("A", 200000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a}, []string{"A"})
	ps := h.prepare(owner, movieKey, "paused-source")
	g := h.m.lookup(ps.ID).current()
	<-g.indexReady
	g.src = pausedRetrySource{g.src}
	a.setFail(120000, 130000, core.ErrSourceUnavailable)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Range", "bytes=120000-120999")
	w := httptest.NewRecorder()
	h.m.ServeRange(w, req, ps.ID)
	if w.Code != http.StatusConflict {
		t.Fatalf("paused source status %d", w.Code)
	}
	if _, ok := h.m.Session(ps.ID); ok {
		t.Fatal("paused source remained streaming")
	}
}
