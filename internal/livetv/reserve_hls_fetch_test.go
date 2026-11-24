package livetv

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A provider segment that fails goes straight back to ffmpeg, so its HLS
// demuxer skips it while the reserve plays: no refusal pacing on the
// segment, and none inherited by the playlist reload that follows.
func TestRelayFailsHLSSegmentFast(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\ns1.ts\n#EXTINF:4,\ns2.ts\n#EXTINF:4,\ns3.ts\n")
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ss := &session{ctx: ctx, cancel: cancel, info: SessionInfo{ID: "steady-h", Channel: Channel{ID: "h", Kind: "iptv"}}}
	if err := s.grantSteadyInput(ss, StreamSource{ID: "a", URL: up.URL + "/live.m3u8"}, http.Header{}); err != nil {
		t.Fatal(err)
	}
	defer s.revokeInput(ss)
	input, _ := s.relayInput(ss)
	get := func(u string) (int, string) {
		res, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(b)
	}
	_, playlist := get(input)
	segs := regexp.MustCompile(`http://\S+`).FindAllString(playlist, -1)
	if len(segs) == 0 {
		t.Fatalf("no segments in %q", playlist)
	}
	for range 2 {
		code, _ := get(segs[0])
		if code < 500 {
			t.Fatalf("failed segment: %d", code)
		}
	}
	if _, body := get(input); !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("playlist reload after failed segments returned %q", body)
	}
}

// When the decoder reaches a segment still in flight and a later one is
// ready, the stuck one is skipped instead of starving the decoder.
func TestSteadySkipsStuckHLSSegment(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live.m3u8":
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n")
			for i := 1; i <= 10; i++ {
				fmt.Fprintf(&b, "#EXTINF:2,\ns%d.ts\n", i)
			}
			io.WriteString(w, b.String())
		case "/s6.ts":
			<-r.Context().Done() // never answers
		default:
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(make([]byte, 188*100))
		}
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client(), Stability: func() string { return stabilityBalanced }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ss := &session{ctx: ctx, cancel: cancel, info: SessionInfo{ID: "steady-skip", Channel: Channel{ID: "skip", Kind: "iptv"}}}
	if err := s.grantSteadyInput(ss, StreamSource{ID: "a", URL: up.URL + "/live.m3u8"}, http.Header{}); err != nil {
		t.Fatal(err)
	}
	defer s.revokeInput(ss)
	input, _ := s.relayInput(ss)
	res, err := http.Get(input)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	segs := regexp.MustCompile(`http://\S+`).FindAllString(string(b), -1)
	if r, err := http.Get(segs[0]); err != nil || r.StatusCode != 200 {
		t.Fatalf("s5: %v", err)
	} else {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	r, err := http.Get(segs[1]) // s6, stuck
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("stuck segment: %d", r.StatusCode)
	}
	if r, err := http.Get(segs[2]); err != nil || r.StatusCode != 200 {
		t.Fatalf("s7 after the skip: %v", err)
	} else {
		r.Body.Close()
	}
}

// On an account sold by the connection, prefetch is strictly sequential:
// never two provider requests at once for one channel.
func TestSteadyPrefetchRespectsAccountLimit(t *testing.T) {
	var inFlight, peak, segments atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live.m3u8" {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:1\n")
			for i := 1; i <= 10; i++ {
				fmt.Fprintf(&b, "#EXTINF:2,\ns%d.ts\n", i)
			}
			io.WriteString(w, b.String())
			return
		}
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		segments.Add(1)
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(make([]byte, 188*100))
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client(), Stability: func() string { return stabilityBalanced }})
	bg := context.Background()
	if _, err := s.ImportSource(bg, Source{ID: "capped", Name: "One connection", Playlist: "#EXTM3U\n#EXTINF:-1,One\n" + up.URL + "/live.m3u8\n"}); err != nil {
		t.Fatal(err)
	}
	entry, err := s.sourceEntry(bg, "capped")
	if err != nil {
		t.Fatal(err)
	}
	entry.limit = 1
	s.authCache.mu.Lock()
	s.authCache.m["capped"] = entry
	s.authCache.mu.Unlock()
	// The Steady channel holds the account's one connection, as a broadcast
	// does in production.
	release, err := s.reserveBroadcastProvider("capped", "capped", "broadcast-capped", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ss := &session{ctx: ctx, cancel: cancel, info: SessionInfo{ID: "steady-capped", Channel: Channel{ID: "capped", Kind: "iptv", SourceID: "capped"}}}
	if err := s.grantSteadyInput(ss, StreamSource{ID: "a", URL: up.URL + "/live.m3u8", AuthSourceID: "capped"}, http.Header{}); err != nil {
		t.Fatal(err)
	}
	defer s.revokeInput(ss)
	input, _ := s.relayInput(ss)
	res, err := http.Get(input)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	segs := regexp.MustCompile(`http://\S+`).FindAllString(string(b), -1)
	r, err := http.Get(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	for deadline := time.Now().Add(5 * time.Second); segments.Load() < 4 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	if segments.Load() < 4 {
		t.Fatalf("prefetch fetched %d segments", segments.Load())
	}
	if p := peak.Load(); p != 1 {
		t.Fatalf("%d provider requests at once on a one-connection account", p)
	}
}

// A stop while segments are still queued ends the prefetch worker: the
// queued fetches are dropped with the grant, not retried forever.
func TestSteadyPrefetchWorkerStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &inputGrant{reserve: time.Second, ctx: ctx, cancel: cancel}
	h := g.hlsState()
	h.mu.Lock()
	h.queue = append(h.queue, &hlsFetch{done: make(chan struct{})}, &hlsFetch{done: make(chan struct{})})
	h.fetching = true
	h.mu.Unlock()
	cancel()
	done := make(chan struct{})
	go h.work(func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Let the worker out so it does not outlive the test.
		h.mu.Lock()
		h.queue = nil
		h.mu.Unlock()
		<-done
		t.Fatal("prefetch worker still running 2s after the grant was cancelled")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fetching {
		t.Fatal("fetching still set after the worker returned")
	}
}
