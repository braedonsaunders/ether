package usenet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
	"github.com/braedonsaunders/ether/internal/yenc"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newFetcher(t *testing.T, providers []config.UsenetProvider, o Options) *Fetcher {
	t.Helper()
	if o.Logger == nil {
		o.Logger = quiet
	}
	// Tests assert exact lazy dial/idle counts, so the warm floor is opt-in
	// here: a test that wants it sets MinIdle explicitly.
	if o.MinIdle == 0 {
		o.MinIdle = -1
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = 2 * time.Second
	}
	if o.CommandTimeout == 0 {
		o.CommandTimeout = 5 * time.Second
	}
	f := New(providers, o)
	t.Cleanup(func() { f.Close() })
	return f
}

func addSeg(t *testing.T, s *nntptest.Server, id string, data []byte) {
	t.Helper()
	if _, err := s.AddYenc(id, data, yenc.EncodeOptions{Name: "f.bin", Part: 1, Total: 1, FileSize: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
}

func statsFor(f *Fetcher, id string) ProviderStats {
	for _, st := range f.Stats() {
		if st.ID == id {
			return st
		}
	}
	return ProviderStats{}
}

func TestFetchPostedFileTLSAuth(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{Username: "u", Password: "p", TLS: true})
	file := randData(10, 2_500_000)
	post, err := s.Post(map[string][]byte{"Movie.2024.mkv": file}, 768_000, nntptest.PostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f := newFetcher(t, []config.UsenetProvider{s.Provider("primary", 4)}, Options{TLSConfig: s.ClientTLSConfig()})
	var got []byte
	segs := post.Files[0].Segments
	for _, sg := range segs {
		data, part, err := f.BodyPart(context.Background(), sg.MessageID, []string{"alt.binaries.test"}, core.PriorityLive)
		if err != nil {
			t.Fatal(err)
		}
		want := core.ArticlePart{Name: "Movie.2024.mkv", FileSize: int64(len(file)), Part: sg.Number, Total: len(segs), Begin: sg.Begin, End: sg.End}
		if part != want {
			t.Fatalf("part %+v, want %+v", part, want)
		}
		got = append(got, data...)
	}
	if !bytes.Equal(got, file) {
		t.Fatal("file mismatch")
	}
	st := statsFor(f, "primary")
	if st.Articles != int64(len(segs)) || st.Bytes < int64(len(file)) || st.State != StateOK || st.Dials != 1 {
		t.Fatalf("stats %+v", st)
	}
	// A loopback round trip can measure as zero on a coarse clock (Windows).
	if st.AvgLatency < 0 || st.AvgFetchTime <= 0 || st.LiveConnections != 1 || st.IdleConnections != 1 {
		t.Fatalf("stats %+v", st)
	}
	// Body is BodyPart without the header.
	data, err := f.Body(context.Background(), "<"+segs[0].MessageID+">", nil, core.PriorityReadahead)
	if err != nil || !bytes.Equal(data, file[:768_000]) {
		t.Fatalf("Body: %v", err)
	}
}

func TestSinglePartArticlePart(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	_, _ = s.AddYenc("single@x", []byte("hello"), yenc.EncodeOptions{Name: "h.txt"})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 1)}, Options{})
	_, part, err := f.BodyPart(context.Background(), "single@x", nil, core.PriorityLive)
	if err != nil {
		t.Fatal(err)
	}
	if part != (core.ArticlePart{Name: "h.txt", FileSize: 5, Part: 1, Total: 1, Begin: 1, End: 5}) {
		t.Fatalf("part %+v", part)
	}
}

func TestPriorityAndBackfill(t *testing.T) {
	primary := nntptest.Start(t, nntptest.Options{})
	secondary := nntptest.Start(t, nntptest.Options{})
	block := nntptest.Start(t, nntptest.Options{})
	a, b, c := randData(1, 1000), randData(2, 1000), randData(3, 1000)
	addSeg(t, primary, "a@x", a)
	addSeg(t, secondary, "a@x", a)
	addSeg(t, block, "a@x", a)
	addSeg(t, secondary, "b@x", b)
	addSeg(t, block, "b@x", b)
	addSeg(t, block, "c@x", c)

	pp := primary.Provider("primary", 2)
	sp := secondary.Provider("secondary", 2)
	sp.Priority = 1
	bp := block.Provider("block", 2)
	bp.Backfill = true
	bp.Priority = -5 // backfill is always last, whatever its priority
	f := newFetcher(t, []config.UsenetProvider{bp, sp, pp}, Options{})

	ctx := context.Background()
	if got, err := f.Body(ctx, "a@x", nil, core.PriorityLive); err != nil || !bytes.Equal(got, a) {
		t.Fatalf("a: %v", err)
	}
	if secondary.BodyRequests("a@x") != 0 || block.BodyRequests("a@x") != 0 {
		t.Fatal("primary has a; others must not be asked")
	}
	if got, err := f.Body(ctx, "b@x", nil, core.PriorityLive); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("b: %v", err)
	}
	if primary.BodyRequests("b@x") != 1 || block.BodyRequests("b@x") != 0 {
		t.Fatal("b should come from secondary without touching backfill")
	}
	if got, err := f.Body(ctx, "c@x", nil, core.PriorityLive); err != nil || !bytes.Equal(got, c) {
		t.Fatalf("c: %v", err)
	}
	if primary.BodyRequests("c@x") != 1 || secondary.BodyRequests("c@x") != 1 || block.BodyRequests("c@x") != 1 {
		t.Fatal("c should be tried on every provider in order")
	}
	// Remembered misses: fetching c again skips primary and secondary.
	if _, err := f.Body(ctx, "c@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	if primary.BodyRequests("c@x") != 1 || secondary.BodyRequests("c@x") != 1 || block.BodyRequests("c@x") != 2 {
		t.Fatal("miss cache not used")
	}
	if st := statsFor(f, "primary"); st.Misses != 2 {
		t.Fatalf("primary misses %d", st.Misses)
	}
}

func TestMissingEverywhere(t *testing.T) {
	s1 := nntptest.Start(t, nntptest.Options{})
	s2 := nntptest.Start(t, nntptest.Options{})
	p2 := s2.Provider("b", 1)
	p2.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{s1.Provider("a", 1), p2}, Options{})
	_, err := f.Body(context.Background(), "gone@x", nil, core.PriorityLive)
	if !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v", err)
	}
	_, err = f.Body(context.Background(), "gone@x", nil, core.PriorityLive)
	if !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v", err)
	}
	if s1.BodyRequests("gone@x") != 1 || s2.BodyRequests("gone@x") != 1 {
		t.Fatal("retry re-hammered providers")
	}
	ok, err := f.Stat(context.Background(), "gone@x")
	if err != nil || ok {
		t.Fatalf("stat %v %v", ok, err)
	}
	if s1.Stats().Commands["STAT"] != 0 {
		t.Fatal("stat should use the miss cache")
	}
}

func TestMissCacheExpires(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("a", 1)}, Options{MissTTL: 30 * time.Millisecond})
	data := randData(5, 100)
	if _, err := f.Body(context.Background(), "late@x", nil, core.PriorityLive); !errors.Is(err, core.ErrArticleMissing) {
		t.Fatal(err)
	}
	addSeg(t, s, "late@x", data)
	time.Sleep(50 * time.Millisecond)
	if got, err := f.Body(context.Background(), "late@x", nil, core.PriorityLive); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestDownProviderIsNotMissing(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	down := config.UsenetProvider{ID: "down", Host: "127.0.0.1", Port: port, Connections: 1, Enabled: true}
	s := nntptest.Start(t, nntptest.Options{})
	bp := s.Provider("block", 1)
	bp.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{down, bp}, Options{BackoffMin: time.Hour, FailThreshold: 1})
	_, err := f.Body(context.Background(), "x@y", nil, core.PriorityLive)
	if err == nil || errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("an unreachable provider might have it: err = %v", err)
	}
	if st := statsFor(f, "down"); st.State != StateBackoff || st.LastError == "" || st.Errors == 0 {
		t.Fatalf("stats %+v", st)
	}
	// While backing off it is skipped without dialling but still counts.
	_, err = f.Body(context.Background(), "x@y", nil, core.PriorityLive)
	if !errors.Is(err, ErrProviderBackoff) || errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v", err)
	}
	// Articles the backfill has are still served.
	addSeg(t, s, "have@y", []byte("ok"))
	if got, err := f.Body(context.Background(), "have@y", nil, core.PriorityLive); err != nil || string(got) != "ok" {
		t.Fatalf("backfill: %v", err)
	}
}

func TestAuthFailureDisablesUntilConfigChanges(t *testing.T) {
	bad := nntptest.Start(t, nntptest.Options{Username: "u", Password: "right"})
	good := nntptest.Start(t, nntptest.Options{})
	addSeg(t, bad, "a@x", []byte("from-bad"))
	addSeg(t, good, "b@x", []byte("from-good"))
	bp := bad.Provider("bad", 2)
	bp.Password = "wrong"
	gp := good.Provider("good", 2)
	gp.Priority = 1
	f := newFetcher(t, []config.UsenetProvider{bp, gp}, Options{})

	if got, err := f.Body(context.Background(), "b@x", nil, core.PriorityLive); err != nil || string(got) != "from-good" {
		t.Fatalf("b: %v", err)
	}
	if st := statsFor(f, "bad"); st.State != StateAuthFail {
		t.Fatalf("state %q", st.State)
	}
	fails := bad.Stats().AuthFailures
	// Locked-out providers cannot answer, so a 430 from the rest is final,
	// and the bad provider is not dialled again.
	if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v", err)
	}
	if bad.Stats().AuthFailures != fails {
		t.Fatal("disabled provider was dialled again")
	}
	// Same config again keeps the lockout.
	f.Update([]config.UsenetProvider{bp, gp})
	if st := statsFor(f, "bad"); st.State != StateAuthFail {
		t.Fatalf("state after identical update %q", st.State)
	}
	// Fixing the password re-enables it.
	bp.Password = "right"
	f.Update([]config.UsenetProvider{bp, gp})
	if st := statsFor(f, "bad"); st.State != StateOK {
		t.Fatalf("state after fix %q", st.State)
	}
	if got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err != nil || string(got) != "from-bad" {
		t.Fatalf("a after fix: %v", err)
	}
}

func TestOnlyProviderAuthFails(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{Username: "u", Password: "p"})
	p := s.Provider("only", 1)
	p.Password = "nope"
	f := newFetcher(t, []config.UsenetProvider{p}, Options{})
	_, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	if !errors.Is(err, ErrNoProviders) || errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoProviders(t *testing.T) {
	f := newFetcher(t, []config.UsenetProvider{{Host: "x", Enabled: false}}, Options{})
	if _, err := f.Body(context.Background(), "a@b", nil, core.PriorityLive); !errors.Is(err, ErrNoProviders) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.Body(context.Background(), "a b", nil, core.PriorityLive); !errors.Is(err, ErrInvalidMessageID) {
		t.Fatalf("err = %v", err)
	}
	if len(f.Stats()) != 0 || f.MaxConnections() != 0 {
		t.Fatal("disabled provider listed")
	}
}

func TestConnectionLimit(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{Latency: 5 * time.Millisecond})
	for i := 0; i < 40; i++ {
		addSeg(t, s, fmt.Sprintf("s%d@x", i), randData(int64(i), 20_000))
	}
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 3)}, Options{})
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			prio := core.Priority(i % 3)
			if _, err := f.Body(context.Background(), fmt.Sprintf("s%d@x", i), nil, prio); err != nil {
				failures.Add(1)
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.FailNow()
	}
	st := s.Stats()
	if st.MaxActive > 3 || st.Rejected != 0 {
		t.Fatalf("server saw %d concurrent connections (limit 3), %d rejected", st.MaxActive, st.Rejected)
	}
	if ps := statsFor(f, "p"); ps.LiveConnections > 3 || ps.ActiveConnections != 0 || ps.Articles != 40 {
		t.Fatalf("stats %+v", ps)
	}
}

func TestCancellationReleasesSlot(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("a"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 1)}, Options{})
	// Warm a connection, then make the server slow.
	if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	s.SetLatency(400 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	inflight := make(chan error, 1)
	go func() {
		_, err := f.Body(ctx, "a@x", nil, core.PriorityLive)
		inflight <- err
	}()
	// A queued request behind it, cancelled while waiting.
	time.Sleep(10 * time.Millisecond)
	ctx2, cancel2 := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		_, err := f.Body(ctx2, "a@x", nil, core.PriorityPrefetch)
		queued <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel2()
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued err = %v", err)
	}
	start := time.Now()
	if err := <-inflight; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inflight err = %v", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("in-flight request not interrupted promptly")
	}
	// Caller cancellation returns immediately; the scheduler joins the socket
	// cancellation asynchronously so independent pipelined callers can survive.
	deadline := time.Now().Add(200 * time.Millisecond)
	st := statsFor(f, "p")
	for st.ActiveConnections != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		st = statsFor(f, "p")
	}
	if st.ActiveConnections != 0 || st.WaitingPrefetch != 0 || st.LiveConnections != 0 {
		t.Fatalf("slots not released / broken conn kept: %+v", st)
	}
	if st.Errors != 0 {
		t.Fatalf("cancellation counted as provider error: %+v", st)
	}
	s.SetLatency(0)
	if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
}

func TestPrefetchYieldsToLive(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{Latency: 60 * time.Millisecond})
	for i := 0; i < 6; i++ {
		addSeg(t, s, fmt.Sprintf("p%d@x", i), []byte("x"))
	}
	addSeg(t, s, "live@x", []byte("live"))
	// Two connections; prefetch may hold at most one of them.
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 2)}, Options{PrefetchFraction: 0.5})

	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.Body(context.Background(), fmt.Sprintf("p%d@x", i), nil, core.PriorityPrefetch); err != nil {
				t.Error(err)
			}
			record("prefetch")
		}(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for statsFor(f, "p").WaitingPrefetch < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("prefetch not queued: %+v", statsFor(f, "p"))
		}
		time.Sleep(time.Millisecond)
	}
	if st := statsFor(f, "p"); st.PrefetchActive != 1 {
		t.Fatalf("prefetch active %d, want 1", st.PrefetchActive)
	}
	// A live request gets the free connection immediately rather than
	// queueing behind five prefetches.
	start := time.Now()
	if _, err := f.Body(context.Background(), "live@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	record("live")
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("live request took %v behind prefetch", d)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	pos := -1
	for i, n := range order {
		if n == "live" {
			pos = i
		}
	}
	if pos > 2 {
		t.Fatalf("live finished at position %d: %v", pos, order)
	}
	if s.Stats().MaxActive > 2 {
		t.Fatal("connection limit exceeded")
	}
}

func TestCorruptArticleFallsThrough(t *testing.T) {
	s1 := nntptest.Start(t, nntptest.Options{})
	s2 := nntptest.Start(t, nntptest.Options{})
	data := randData(8, 30_000)
	addSeg(t, s1, "c@x", data)
	addSeg(t, s2, "c@x", data)
	if err := s1.Corrupt("c@x"); err != nil {
		t.Fatal(err)
	}
	p2 := s2.Provider("two", 1)
	p2.Priority = 1
	f := newFetcher(t, []config.UsenetProvider{s1.Provider("one", 1), p2}, Options{})
	got, err := f.Body(context.Background(), "c@x", nil, core.PriorityLive)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("err = %v", err)
	}
	if s1.BodyRequests("c@x") != 1 {
		t.Fatal("corrupt article should not be retried on the same provider")
	}
	if st := statsFor(f, "one"); st.Errors != 1 || st.LiveConnections != 1 {
		t.Fatalf("stats %+v (connection should survive a corrupt article)", st)
	}

	// Corrupt everywhere: ErrCorrupt, not missing.
	_ = s2.Corrupt("c@x")
	_, err = f.Body(context.Background(), "c@x", nil, core.PriorityLive)
	if !errors.Is(err, ErrCorrupt) || errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v", err)
	}
}

func TestReconnectAfterServerDrop(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("alpha"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 4)}, Options{})
	// Explicitly warm several sockets: duplicate BODY requests now coalesce.
	if err := f.Warm(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	s.DropConnections()
	time.Sleep(20 * time.Millisecond)
	got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	if err != nil || string(got) != "alpha" {
		t.Fatalf("after drop: %v", err)
	}
	st := statsFor(f, "p")
	if st.LiveConnections != 1 || st.State != StateOK {
		t.Fatalf("stale idle connections not discarded: %+v", st)
	}
}

func TestTransientErrorsRetried(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("alpha"))
	var n atomic.Int32
	s.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd != "BODY" {
			return nntptest.Fault{}
		}
		switch n.Add(1) {
		case 1:
			return nntptest.Fault{Drop: true}
		case 2:
			return nntptest.Fault{Truncate: true}
		}
		return nntptest.Fault{}
	})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 2)}, Options{Retries: 2, FailThreshold: 3})
	got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	if err != nil || string(got) != "alpha" {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts %d", n.Load())
	}

	// Persistent protocol errors exhaust retries and move on, then report
	// a transient (not missing) error.
	s.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" {
			return nntptest.Fault{Code: 503, Text: "program fault"}
		}
		return nntptest.Fault{}
	})
	_, err = f.Body(context.Background(), "a@x", nil, core.PriorityLive)
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != 503 || errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v", err)
	}
}

func TestStat(t *testing.T) {
	s1 := nntptest.Start(t, nntptest.Options{})
	s2 := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s2, "b@x", []byte("b"))
	p2 := s2.Provider("two", 1)
	p2.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{s1.Provider("one", 1), p2}, Options{})
	ok, err := f.Stat(context.Background(), "b@x")
	if err != nil || !ok {
		t.Fatalf("stat b: %v %v", ok, err)
	}
	ok, err = f.Stat(context.Background(), "nope@x")
	if err != nil || ok {
		t.Fatalf("stat nope: %v %v", ok, err)
	}
	s1.Close()
	s2.Close()
	_, err = f.Stat(context.Background(), "other@x")
	if err == nil {
		t.Fatal("stat against dead servers should fail")
	}
}

func TestStatManyPipelinesAndFallsBackToBlockProvider(t *testing.T) {
	s1 := nntptest.Start(t, nntptest.Options{Username: "u", Password: "p"})
	s2 := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s1, "primary@x", []byte("p"))
	addSeg(t, s1, "taken@x", []byte("primary takedown"))
	addSeg(t, s2, "block@x", []byte("b"))
	addSeg(t, s2, "taken@x", []byte("secondary copy"))
	s1.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "STAT" && arg == "taken@x" {
			return nntptest.Fault{Code: 451, Text: "0 <taken@x>"}
		}
		return nntptest.Fault{}
	})
	p2 := s2.Provider("block", 2)
	p2.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{s1.Provider("primary", 4), p2}, Options{})
	if err := f.Warm(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if got := s1.Stats().Accepted; got < 2 {
		t.Fatalf("warm opened %d connections, want at least 2", got)
	}
	got, err := f.StatMany(context.Background(), []string{"primary@x", "block@x", "gone@x", "taken@x"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["primary@x"] || !got["block@x"] || got["gone@x"] || !got["taken@x"] {
		t.Fatalf("results %v", got)
	}
	stats := f.Stats()
	if stats[0].STATBatches == 0 {
		t.Fatalf("primary did not pipeline STATs: %+v", stats[0])
	}
}

func TestPooledStatManyThenBodyAndTimeoutRecovery(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{Username: "user", Password: "pass"})
	addSeg(t, s, "present@x", []byte("stream body"))
	p := s.Provider("primary", 1)
	f := New([]config.UsenetProvider{p}, Options{CommandTimeout: 40 * time.Millisecond, Retries: 0})
	t.Cleanup(func() { _ = f.Close() })
	if err := f.Warm(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	got, err := f.StatMany(context.Background(), []string{"present@x", "missing@x"})
	if err != nil || !got["present@x"] || got["missing@x"] {
		t.Fatalf("STAT result=%v err=%v", got, err)
	}
	body, _, err := f.BodyPart(context.Background(), "present@x", nil, core.PriorityLive)
	if err != nil || string(body) != "stream body" {
		t.Fatalf("BODY=%q err=%v", body, err)
	}
	if dials := f.Stats()[0].Dials; dials != 1 {
		t.Fatalf("STAT->BODY should reuse one pooled connection, dials=%d", dials)
	}

	s.SetFault(func(cmd, _ string) nntptest.Fault {
		if cmd == "STAT" {
			return nntptest.Fault{Delay: 150 * time.Millisecond}
		}
		return nntptest.Fault{}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err = f.StatMany(ctx, []string{"present@x", "missing@x", "present@x"})
	cancel()
	if err == nil {
		t.Fatal("expected STAT pipeline timeout")
	}
	s.SetFault(nil)
	time.Sleep(180 * time.Millisecond) // let the fake server drain the abandoned pipeline
	body, _, err = f.BodyPart(context.Background(), "present@x", nil, core.PriorityLive)
	if err != nil || string(body) != "stream body" {
		t.Fatalf("BODY after pipeline timeout=%q err=%v", body, err)
	}
	if dials := f.Stats()[0].Dials; dials != 2 {
		t.Fatalf("timed-out pipeline connection should be discarded; dials=%d want 2", dials)
	}
}

func TestConcurrentWarmDoesNotMultiplyPerProviderLimit(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{MaxConns: 8})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("primary", 8)}, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.Warm(context.Background(), 4); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	stats := s.Stats()
	if stats.MaxActive > 4 {
		t.Fatalf("concurrent Warm opened %d connections, limit is 4", stats.MaxActive)
	}
	if stats.Active != 4 {
		t.Fatalf("expected four warm idle connections, active=%d", stats.Active)
	}
}

func TestHotReload(t *testing.T) {
	s1 := nntptest.Start(t, nntptest.Options{})
	s2 := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s1, "a@x", []byte("one"))
	addSeg(t, s2, "a@x", []byte("two"))
	p1 := s1.Provider("p1", 2)
	f := newFetcher(t, []config.UsenetProvider{p1}, Options{})
	if got, _ := f.Body(context.Background(), "a@x", nil, core.PriorityLive); string(got) != "one" {
		t.Fatalf("got %q", got)
	}
	before := f.pools[0]

	// Identical config keeps the pool and its connections.
	f.Update([]config.UsenetProvider{p1})
	if f.pools[0] != before || statsFor(f, "p1").Articles != 1 {
		t.Fatal("unchanged provider should keep its pool")
	}

	// Slow in-flight request on p1 while it is removed: it completes.
	s1.SetLatency(150 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
		if err == nil && string(got) != "one" {
			err = fmt.Errorf("got %q", got)
		}
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	f.Update([]config.UsenetProvider{s2.Provider("p2", 2)})
	if got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err != nil || string(got) != "two" {
		t.Fatalf("new provider: %q %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("in-flight request on removed provider: %v", err)
	}
	st := f.Stats()
	if len(st) != 1 || st[0].ID != "p2" {
		t.Fatalf("stats %+v", st)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s1.Stats().Active != 0 {
		if time.Now().After(deadline) {
			t.Fatal("removed provider's connections not closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if before.stats().State != StateClosed {
		t.Fatal("old pool not closed")
	}
}

func TestIdleTimeoutAndKeepAlive(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("a"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 1)}, Options{MinIdle: -1, IdleTimeout: 400 * time.Millisecond, KeepAlive: 60 * time.Millisecond})
	if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if s.Stats().Commands["DATE"] == 0 {
		t.Fatal("no keepalive sent")
	}
	if st := statsFor(f, "p"); st.LiveConnections != 1 {
		t.Fatalf("connection closed too early: %+v", st)
	}
	deadline := time.Now().Add(3 * time.Second)
	for statsFor(f, "p").LiveConnections != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle connection never closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for s.Stats().Commands["QUIT"] == 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle close should QUIT")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBackoffRecovers(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("a"))
	var fail atomic.Bool
	fail.Store(true)
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if fail.Load() {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 1)}, Options{
		DialContext: dialer, BackoffMin: 50 * time.Millisecond, BackoffMax: 100 * time.Millisecond, FailThreshold: 2, Retries: 3,
	})
	if _, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err == nil {
		t.Fatal("expected failure")
	}
	if st := statsFor(f, "p"); st.State != StateBackoff {
		t.Fatalf("state %q", st.State)
	}
	fail.Store(false)
	time.Sleep(120 * time.Millisecond)
	if got, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive); err != nil || string(got) != "a" {
		t.Fatalf("after backoff: %v", err)
	}
	if st := statsFor(f, "p"); st.State != StateOK {
		t.Fatalf("state %q", st.State)
	}
}

func TestSameTierSharesLoad(t *testing.T) {
	s1 := nntptest.Start(t, nntptest.Options{Latency: 20 * time.Millisecond})
	s2 := nntptest.Start(t, nntptest.Options{Latency: 20 * time.Millisecond})
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("s%d@x", i)
		addSeg(t, s1, id, []byte("x"))
		addSeg(t, s2, id, []byte("x"))
	}
	f := newFetcher(t, []config.UsenetProvider{s1.Provider("a", 2), s2.Provider("b", 2)}, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.Body(context.Background(), fmt.Sprintf("s%d@x", i), nil, core.PriorityLive); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	a, b := s1.Stats().ArticlesSent, s2.Stats().ArticlesSent
	if a+b != 20 || a == 0 || b == 0 {
		t.Fatalf("load split %d/%d", a, b)
	}
	if f.MaxConnections() != 4 {
		t.Fatalf("max connections %d", f.MaxConnections())
	}
}

func TestUseGroup(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "a@x", []byte("a"))
	addSeg(t, s, "b@x", []byte("b"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 1)}, Options{UseGroup: true})
	for _, id := range []string{"a@x", "b@x"} {
		if _, err := f.Body(context.Background(), id, []string{"alt.binaries.test"}, core.PriorityLive); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.Stats().Commands["GROUP"]; n != 1 {
		t.Fatalf("GROUP sent %d times, want 1", n)
	}
}

func TestConcurrentStressWithReload(t *testing.T) {
	s1 := nntptest.Start(t, nntptest.Options{})
	s2 := nntptest.Start(t, nntptest.Options{})
	data := map[string][]byte{}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("m%d@x", i)
		data[id] = randData(int64(100+i), 5000+i)
		addSeg(t, s1, id, data[id])
		addSeg(t, s2, id, data[id])
	}
	p1, p2 := s1.Provider("a", 3), s2.Provider("b", 2)
	p2.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{p1, p2}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				id := fmt.Sprintf("m%d@x", (w*7+i)%30)
				got, err := f.Body(ctx, id, nil, core.Priority(i%3))
				if err != nil {
					t.Errorf("%s: %v", id, err)
					continue
				}
				if !bytes.Equal(got, data[id]) {
					t.Errorf("%s: wrong payload", id)
				}
			}
		}(w)
	}
	for i := 0; i < 5; i++ {
		time.Sleep(10 * time.Millisecond)
		p1.Connections = 2 + i%2
		f.Update([]config.UsenetProvider{p1, p2})
		_ = f.Stats()
	}
	wg.Wait()
	if st := s1.Stats(); st.MaxActive > 6 {
		t.Fatalf("too many connections across reloads: %d", st.MaxActive)
	}
}

func TestTestProvider(t *testing.T) {
	ok := nntptest.Start(t, nntptest.Options{Username: "u", Password: "p"})
	tlsSrv := nntptest.Start(t, nntptest.Options{TLS: true})
	f := newFetcher(t, nil, Options{TLSConfig: tlsSrv.ClientTLSConfig(), DialTimeout: 300 * time.Millisecond})
	ctx := context.Background()

	if err := f.TestProvider(ctx, ok.Provider("x", 1)); err != nil {
		t.Fatalf("good provider: %v", err)
	}
	if err := f.TestProvider(ctx, tlsSrv.Provider("x", 1)); err != nil {
		t.Fatalf("tls provider: %v", err)
	}

	kind := func(err error) string {
		var pe *ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("not a ProviderError: %v", err)
		}
		if pe.Message == "" {
			t.Fatal("empty message")
		}
		return pe.Kind
	}
	bad := ok.Provider("x", 1)
	bad.Password = "wrong"
	if k := kind(f.TestProvider(ctx, bad)); k != ProblemAuth {
		t.Fatalf("bad password: %s", k)
	}
	if k := kind(f.TestProvider(ctx, config.UsenetProvider{})); k != ProblemConfig {
		t.Fatalf("empty: %s", k)
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if k := kind(f.TestProvider(ctx, config.UsenetProvider{Host: "127.0.0.1", Port: port})); k != ProblemUnreachable {
		t.Fatalf("refused: %s", k)
	}

	// SSL on against a plain server.
	plainAsTLS := ok.Provider("x", 1)
	plainAsTLS.TLS = true
	err := f.TestProvider(ctx, plainAsTLS)
	if k := kind(err); k != ProblemTLS {
		t.Fatalf("tls against plain: %s (%v)", k, err)
	}
	// SSL off against a TLS server: the server waits for a ClientHello.
	tlsAsPlain := tlsSrv.Provider("x", 1)
	tlsAsPlain.TLS = false
	err = f.TestProvider(ctx, tlsAsPlain)
	if k := kind(err); k != ProblemTimeout && k != ProblemProtocol {
		t.Fatalf("plain against tls: %s (%v)", k, err)
	}
	if !strings.Contains(err.Error(), "SSL") {
		t.Fatalf("message should hint at SSL: %v", err)
	}
	// Untrusted certificate (default roots).
	if k := kind(TestProvider(ctx, tlsSrv.Provider("x", 1))); k != ProblemTLS {
		t.Fatalf("untrusted cert: %s", k)
	}
	// Connection limit at greeting.
	full := nntptest.Start(t, nntptest.Options{MaxConns: -1})
	full.SetMaxConns(1)
	hold := dialServer(t, full)
	defer hold.Close()
	if k := kind(f.TestProvider(ctx, full.Provider("x", 1))); k != ProblemUnavailable {
		t.Fatalf("full: %s", k)
	}
	// DNS failure, via the dialer hook (no real lookups in tests).
	dnsF := newFetcher(t, nil, Options{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: "news.example.invalid", IsNotFound: true}}
	}})
	if k := kind(dnsF.TestProvider(ctx, config.UsenetProvider{Host: "news.example.invalid", Port: 563, TLS: true})); k != ProblemDNS {
		t.Fatalf("dns: %s", k)
	}
}

func TestServerConnectionLimitBelowConfigured(t *testing.T) {
	// The account allows 2 connections but is configured with 4: extra
	// dials get 502 at greeting, yet the provider stays in rotation and
	// requests are served on the connections it does get.
	s := nntptest.Start(t, nntptest.Options{MaxConns: 2, Latency: 10 * time.Millisecond})
	for i := 0; i < 12; i++ {
		addSeg(t, s, fmt.Sprintf("s%d@x", i), []byte("x"))
	}
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 4)}, Options{LimitCooloff: 150 * time.Millisecond, KeepAlive: 20 * time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = f.Body(context.Background(), fmt.Sprintf("s%d@x", i), nil, core.PriorityLive)
		}(i)
	}
	wg.Wait()
	var failed atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.Body(context.Background(), fmt.Sprintf("s%d@x", i), nil, core.PriorityLive); err != nil {
				failed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if st := statsFor(f, "p"); st.State != StateOK {
		t.Fatalf("provider taken out of rotation: %+v", st)
	}
	if s.Stats().MaxActive > 2 {
		t.Fatal("server limit exceeded")
	}
	if failed.Load() != 0 {
		t.Fatalf("%d of 12 requests failed", failed.Load())
	}
	if st := statsFor(f, "p"); st.MaxConnections != 2 || st.ConfiguredConnections != 4 {
		t.Fatalf("pool did not adapt: %+v", st)
	}
	deadline := time.Now().Add(3 * time.Second)
	for statsFor(f, "p").MaxConnections != 4 {
		if time.Now().After(deadline) {
			t.Fatal("configured size never restored")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Giganews answers taken-down articles with "451 0 <id>": the article is
// unavailable there, so the fetcher must fall through to the next provider.
func TestTakedown451FallsThrough(t *testing.T) {
	primary := nntptest.Start(t, nntptest.Options{})
	addSeg(t, primary, "gone@x", []byte("alpha"))
	primary.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" || cmd == "STAT" {
			return nntptest.Fault{Code: 451, Text: "0 <" + arg + ">"}
		}
		return nntptest.Fault{}
	})
	second := nntptest.Start(t, nntptest.Options{})
	addSeg(t, second, "gone@x", []byte("alpha"))
	p1, p2 := primary.Provider("a", 2), second.Provider("b", 2)
	p2.Priority = 1
	f := newFetcher(t, []config.UsenetProvider{p1, p2}, Options{})
	got, err := f.Body(context.Background(), "gone@x", nil, core.PriorityLive)
	if err != nil || string(got) != "alpha" {
		t.Fatalf("body = %q, %v", got, err)
	}
	if ok, err := f.Stat(context.Background(), "gone@x"); err != nil || !ok {
		t.Fatalf("stat = %v, %v", ok, err)
	}

	// Taken down everywhere reads as missing, which triggers release failover.
	only := newFetcher(t, []config.UsenetProvider{p1}, Options{})
	if _, err := only.Body(context.Background(), "gone@x", nil, core.PriorityLive); !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("err = %v, want ErrArticleMissing", err)
	}
}

// NZBs carry message-ids without angle brackets; StatMany must answer under
// the ids exactly as given, or every lookup misses and healthy releases are
// rejected.
func TestStatManyKeysByCallerID(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "here@x", []byte("alpha"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 2)}, Options{})
	got, err := f.StatMany(context.Background(), []string{"here@x", "<here@x>", "gone@x"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["here@x"] || !got["<here@x>"] || got["gone@x"] {
		t.Fatalf("got %v", got)
	}
	if _, ok := got["gone@x"]; !ok {
		t.Fatal("missing ids must be reported as false, not absent")
	}
}

func TestPrimaryIdleFloorSurvivesTimeoutWithoutRedial(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "floor@x", []byte("warm"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("primary", 4)}, Options{MinIdle: 2, IdleTimeout: time.Hour, KeepAlive: time.Second})
	if err := f.Warm(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	pool := f.pools[0]
	agedUsed := time.Now().Add(-2 * time.Second)
	agedIdle := time.Now().Add(-2 * time.Hour)
	pool.mu.Lock()
	if len(pool.idle) < 2 {
		pool.mu.Unlock()
		t.Fatalf("idle=%d", len(pool.idle))
	}
	for _, c := range pool.idle {
		c.used = agedUsed
		c.idleSince = agedIdle
	}
	pool.mu.Unlock()
	pool.sweep()
	st := statsFor(f, "primary")
	if st.LiveConnections != 2 {
		t.Fatalf("warm floor: %+v", st)
	}
	pool.mu.Lock()
	if len(pool.idle) != 2 {
		pool.mu.Unlock()
		t.Fatalf("idle floor=%d", len(pool.idle))
	}
	for _, c := range pool.idle {
		if !c.used.After(agedUsed) {
			t.Errorf("floor connection not kept alive: used %v aged %v", c.used, agedUsed)
		}
	}
	pool.mu.Unlock()
	if s.Stats().Commands["DATE"] < 2 {
		t.Fatalf("warm connections were not pinged: %+v", s.Stats().Commands)
	}
	dials := st.Dials
	if _, err := f.Body(context.Background(), "floor@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	if statsFor(f, "primary").Dials != dials {
		t.Fatal("request redialled the warm floor")
	}
}

func TestTinyBodyCheckBackfillsWhenStatClaimsTakenDownArticleExists(t *testing.T) {
	a := nntptest.Start(t, nntptest.Options{})
	b := nntptest.Start(t, nntptest.Options{})
	addSeg(t, a, "body@x", bytes.Repeat([]byte("a"), 1<<20))
	addSeg(t, b, "body@x", bytes.Repeat([]byte("b"), 1<<20))
	a.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" {
			return nntptest.Fault{Code: 451, Text: "taken down"}
		}
		return nntptest.Fault{}
	})
	pa, pb := a.Provider("p", 2), b.Provider("b", 2)
	pb.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{pa, pb}, Options{})
	if ok, err := f.Stat(context.Background(), "body@x"); err != nil || !ok {
		t.Fatalf("STAT fixture: %v %v", ok, err)
	}
	if err := f.CheckBody(context.Background(), "body@x", nil, core.PriorityLive); err != nil {
		t.Fatal(err)
	}
	if statsFor(f, "b").Bytes > 2*readBufferSize {
		t.Fatal("BODY check downloaded the full article")
	}
	b.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" {
			return nntptest.Fault{Code: 451, Text: "taken down"}
		}
		return nntptest.Fault{}
	})
	if err := f.CheckBody(context.Background(), "body@x", nil, core.PriorityLive); !errors.Is(err, core.ErrArticleMissing) {
		t.Fatalf("global BODY absence not detected: %v", err)
	}
}

// Warm holds every slot it takes until all its connections are up, so it
// must not ask for more slots than a server-limited semaphore has.
func TestWarmWithShrunkSemaphoreDoesNotDeadlock(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 4)}, Options{MinIdle: -1})
	f.pools[0].shrinkTo(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.Warm(ctx, 4); err != nil {
		t.Fatalf("Warm = %v", err)
	}
}
