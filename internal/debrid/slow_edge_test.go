package debrid

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestStreamingTransportTimeoutsTolerateSlowEdge(t *testing.T) {
	tr := NewStreamingTransport()
	if tr.TLSHandshakeTimeout != 15*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %v, want 15s", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != 15*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 15s", tr.ResponseHeaderTimeout)
	}
	if mediaDialTimeout != 10*time.Second {
		t.Fatalf("mediaDialTimeout = %v, want 10s", mediaDialTimeout)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHedgeWaitsForPrimaryConnectionSetup(t *testing.T) {
	r := newEngineRig(t, testBlock, 25*time.Millisecond)
	cfg := r.svc.Config()
	cfg.ReadAheadChunks = -1
	r.svc.UpdateConfig(cfg)

	var primaryCalls, hedgeCalls atomic.Int32
	var hedgeDuringHandshake, hedgeBeforeConnect atomic.Bool
	primaryConnected := make(chan struct{})
	var connectOnce sync.Once

	r.eng.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call := primaryCalls.Add(1)
		tr := httptrace.ContextClientTrace(req.Context())
		if tr != nil && tr.TLSHandshakeStart != nil {
			tr.TLSHandshakeStart()
		}
		// Hold in handshake longer than HedgeDelay (25ms): hedge must NOT fire yet.
		time.Sleep(80 * time.Millisecond)
		if call == 1 && hedgeCalls.Load() != 0 {
			hedgeDuringHandshake.Store(true)
		}
		if tr != nil && tr.GotConn != nil {
			tr.GotConn(httptrace.GotConnInfo{})
		}
		connectOnce.Do(func() { close(primaryConnected) })
		// Now stall after connection setup so the post-connect hedge rescues the read.
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}

	r.eng.hedge = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hedgeCalls.Add(1)
		select {
		case <-primaryConnected:
		default:
			hedgeBeforeConnect.Store(true)
		}
		body := r.media.data[:testBlock]
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			ContentLength: int64(len(body)),
			Body:          io.NopCloser(bytes.NewReader(body)),
			Request:       req,
		}, nil
	})}

	src, err := r.eng.Open(context.Background(), r.rel.ID, r.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	buf := make([]byte, 1024)
	if _, err := src.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, r.media.data[:1024]) {
		t.Fatal("content mismatch")
	}
	if hedgeDuringHandshake.Load() {
		t.Fatal("hedge fired while primary was still handshaking")
	}
	if hedgeBeforeConnect.Load() {
		t.Fatal("hedge started before primary finished connecting")
	}
	if primaryCalls.Load() != 1 || hedgeCalls.Load() != 1 {
		t.Fatalf("primaryCalls=%d hedgeCalls=%d, want 1 and 1", primaryCalls.Load(), hedgeCalls.Load())
	}
}

func TestHostBackoffSpacesFailuresAndResetsOnSuccess(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	b := newHostBackoff(nil)
	b.base = 10 * time.Millisecond
	b.max = 40 * time.Millisecond
	b.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	if d := b.delay(1); d != 10*time.Millisecond {
		t.Fatalf("delay(1) = %v, want 10ms", d)
	}
	if d := b.delay(2); d != 20*time.Millisecond {
		t.Fatalf("delay(2) = %v, want 20ms", d)
	}
	if d := b.delay(5); d != 40*time.Millisecond {
		t.Fatalf("delay(5) = %v, want capped 40ms", d)
	}

	b.failure("slow.example:443")
	if got := b.failureCount("slow.example:443"); got != 1 {
		t.Fatalf("failureCount = %d, want 1", got)
	}
	// Other hosts are unaffected.
	if err := b.wait(context.Background(), "fast.example:443"); err != nil {
		t.Fatalf("other host wait: %v", err)
	}
	// Same host waits for backoff window.
	start := time.Now()
	if err := b.wait(context.Background(), "slow.example:443"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 8*time.Millisecond {
		t.Fatalf("wait returned too early: %v", elapsed)
	}

	// Success clears failure count and backoff immediately.
	b.failure("slow.example:443")
	advance(0)
	b.success("slow.example:443")
	if got := b.failureCount("slow.example:443"); got != 0 {
		t.Fatalf("failureCount after success = %d, want 0", got)
	}
}

func TestConnCapReducesOnResetAnd429AndRestoresAfterCooldown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	c := newConnCap(6)
	c.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	addr := "edge.example:443"
	c.reduce(addr)

	// Reduced from 6 to 3 during cooldown.
	for i := 0; i < 3; i++ {
		if err := c.acquire(context.Background(), addr); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.acquire(ctx, addr); err == nil {
		t.Fatal("4th connection admitted while cap is reduced to 3")
	}

	// After resetCooldown expires, the 4th connection is admitted.
	advance(resetCooldown + time.Second)
	if err := c.acquire(context.Background(), addr); err != nil {
		t.Fatalf("acquire after cooldown: %v", err)
	}
	for i := 0; i < 4; i++ {
		c.release(addr)
	}
}

func TestEngineReducesHostCapOnResetAnd429(t *testing.T) {
	r := newEngineRig(t, 256<<10, -1)
	if !isConnReset(syscall.ECONNRESET) {
		t.Fatal("syscall.ECONNRESET not recognized")
	}
	if !isConnReset(&urlErrorStub{"read tcp: connection reset by peer"}) {
		t.Fatal("connection reset string not recognized")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p, err := DecodePayload(r.pl)
	if err != nil {
		t.Fatal(err)
	}
	p.URL = srv.URL + "/m/realdebrid-1"
	src, err := r.eng.Open(context.Background(), r.rel.ID, p.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_, _ = src.ReadAt(make([]byte, 64), 0)

	addr := dialAddr(p.URL)
	if !strings.Contains(addr, "127.0.0.1:") {
		t.Fatalf("dialAddr(%q) = %q", p.URL, addr)
	}
	r.eng.limit.mu.Lock()
	lim, _ := r.eng.limit.limitLocked(addr)
	r.eng.limit.mu.Unlock()
	if lim >= defaultHostConnections {
		t.Fatalf("cap for %s after 429 = %d, want reduced < %d", addr, lim, defaultHostConnections)
	}
}

type urlErrorStub struct{ msg string }

func (e *urlErrorStub) Error() string { return e.msg }
