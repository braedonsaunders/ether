package debrid

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestCDNRateLimitFailsFastAndSharesRetryAfterAcrossFiles(t *testing.T) {
	r := newEngineRig(t, 1<<20, -1)
	clock := &testClock{t: time.Now()}
	r.eng.now = clock.now
	var calls atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.(http.Flusher).Flush()
		// A rate-limited CDN may never finish its error body. Only the
		// headers are needed; closing the body cancels this handler.
		<-req.Context().Done()
	}))
	defer cdn.Close()
	p, err := DecodePayload(r.pl)
	if err != nil {
		t.Fatal(err)
	}
	p.URL = cdn.URL + "/first?signature=secret"
	f, err := r.eng.acquireFile(t.Context(), "first", p.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer r.eng.releaseFile(f)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err = f.rangeGet(ctx, 0, 100, false)
	if !errors.Is(err, core.ErrSourceUnavailable) || !errors.Is(err, ErrRateLimited) || calls.Load() != 1 {
		t.Fatalf("rate limit retried or misclassified: calls=%d err=%v", calls.Load(), err)
	}
	if ctx.Err() != nil {
		t.Fatal("reading the rate-limit body exhausted the caller's deadline")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("signed URL leaked into error")
	}
	if errors.Is(err, core.ErrArticleMissing) || errors.Is(err, core.ErrReleaseRejected) {
		t.Fatal("temporary CDN limit became permanent")
	}
	p.URL = cdn.URL + "/another?signature=other"
	other, err := r.eng.acquireFile(ctx, "second", p.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer r.eng.releaseFile(other)
	_, _, err = other.rangeGet(ctx, 0, 100, false)
	if !errors.Is(err, ErrRateLimited) || calls.Load() != 1 {
		t.Fatalf("parallel file ignored Retry-After: %v calls=%d", err, calls.Load())
	}
	clock.add(61 * time.Second)
	_, _, _ = other.rangeGet(ctx, 0, 100, false)
	if calls.Load() != 2 {
		t.Fatalf("cooldown never recovered: %d", calls.Load())
	}
	if err := r.eng.checkMediaPause("another-account", p.URL); err != nil {
		t.Fatal("one account paused another")
	}
	if err := r.eng.checkMediaPause(p.Provider, "https://other-cdn.invalid/file"); err != nil {
		t.Fatal("one CDN paused another")
	}
}

func TestSharedCDNPauseDoesNotInterruptAnEstablishedHealthyFile(t *testing.T) {
	r := newEngineRig(t, 1<<20, -1)
	f, err := r.eng.acquireFile(t.Context(), "established", r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer r.eng.releaseFile(f)
	body, _, err := f.rangeGet(t.Context(), 0, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	body.Close()
	p, _ := f.payload()
	_ = r.eng.pauseMedia(p.Provider, p.URL, http.StatusTooManyRequests, time.Minute)
	body, _, err = f.rangeGet(t.Context(), 101, 200, false)
	if err != nil {
		t.Fatalf("another file's rate limit stopped a healthy viewer: %v", err)
	}
	body.Close()
	_ = f.pauseMedia(p.Provider, p.URL, http.StatusTooManyRequests, time.Minute)
	if err := f.checkMediaPause(p.Provider, p.URL); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("established file ignored its own rate limit: %v", err)
	}
}


func TestRateLimitedSourceDisablesDirectPlayRetryAndReadAhead(t *testing.T) {
	r := newEngineRig(t, 4<<20, -1)
	src, err := r.eng.Open(t.Context(), r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	hs := src.(*httpSource)
	if !hs.RetryDirectPlay() {
		t.Fatal("expected healthy httpSource to allow direct play retry")
	}
	p, _ := hs.f.payload()
	_ = hs.f.pauseMedia(p.Provider, p.URL, http.StatusTooManyRequests, 3*time.Minute)
	if hs.RetryDirectPlay() {
		t.Fatal("expected rate-limited httpSource to disable direct play retry so playback fails over immediately")
	}
}
