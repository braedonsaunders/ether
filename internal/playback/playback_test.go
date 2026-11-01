package playback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// ---------------------------------------------------------------------------
// Estimate

func TestEstimate(t *testing.T) {
	q := config.DefaultQuality()
	p4k, p1080, p720 := q.Profiles[0], q.Profiles[1], q.Profiles[2]
	movie := &core.Title{Type: core.TypeMovie, Runtime: 120 * time.Minute, Language: "en"}

	t.Run("1080p h264 eac3", func(t *testing.T) {
		info := EstimateInfo(movie, defaultCaps("c"), p1080)
		v := info.Video()
		if info.Container != "mkv" || !info.Estimated || info.Duration != 120*time.Minute {
			t.Fatalf("info = %+v", info)
		}
		if v.Codec != "h264" || v.Height != 1080 || v.Width != 1920 {
			t.Fatalf("video = %+v", v)
		}
		a := info.Streams[1]
		if a.Codec != "eac3" || a.Channels != 6 || a.Language != "en" {
			t.Fatalf("audio = %+v", a)
		}
		if info.BitrateKbps != 8000+640 {
			t.Fatalf("bitrate = %d", info.BitrateKbps)
		}
		wantSize := int64(float64(info.BitrateKbps) * 1000 / 8 * (120 * time.Minute).Seconds())
		if info.Size != wantSize {
			t.Fatalf("size = %d, want %d", info.Size, wantSize)
		}
	})
	t.Run("4K hevc hdr", func(t *testing.T) {
		caps := core.ClientCaps{MaxHeight: 2160, HDR10: true, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"eac3"}}
		info := EstimateInfo(movie, caps, p4k)
		v := info.Video()
		if v.Codec != "hevc" || v.Height != 2160 || v.ColorTransfer != "smpte2084" || v.BitDepth != 10 {
			t.Fatalf("video = %+v", v)
		}
	})
	t.Run("client cap and aac", func(t *testing.T) {
		caps := core.ClientCaps{MaxHeight: 720, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}
		info := EstimateInfo(movie, caps, p1080)
		if v := info.Video(); v.Height != 720 || v.Width != 1280 {
			t.Fatalf("video = %+v", v)
		}
		if a := info.Streams[1]; a.Codec != "aac" || a.Channels != 2 {
			t.Fatalf("audio = %+v", a)
		}
	})
	t.Run("size bounds clamp bitrate", func(t *testing.T) {
		p := p720
		p.MaxSizeMBPerMin = 10 // ~1398 kbps total
		info := EstimateInfo(movie, defaultCaps("c"), p)
		if info.BitrateKbps != mbPerMinToKbps(10) {
			t.Fatalf("bitrate = %d, want %d", info.BitrateKbps, mbPerMinToKbps(10))
		}
	})
	t.Run("default runtime", func(t *testing.T) {
		info := EstimateInfo(&core.Title{Type: core.TypeEpisode}, defaultCaps("c"), p1080)
		if info.Duration != defaultEpisodeRuntime {
			t.Fatalf("duration = %v", info.Duration)
		}
	})
	t.Run("manager uses SelectProfile", func(t *testing.T) {
		h := newHarness(t, nil, nil)
		caps := core.ClientCaps{MaxHeight: 2160, VideoCodecs: []string{"hevc"}}
		info := h.m.Estimate(context.Background(), movie, caps)
		if v := info.Video(); v.Height != 2160 || v.Codec != "hevc" {
			t.Fatalf("video = %+v", v)
		}
	})
}

// ---------------------------------------------------------------------------
// Time mapping

func TestTimeMap(t *testing.T) {
	m := TimeMap{Size: 1000, Duration: 100 * time.Second}
	if got := m.TimeAt(250); got != 25*time.Second {
		t.Fatalf("TimeAt(250) = %v", got)
	}
	if got := m.OffsetAt(75 * time.Second); got != 750 {
		t.Fatalf("OffsetAt(75s) = %d", got)
	}
	if m.TimeAt(-5) != 0 || m.TimeAt(5000) != 100*time.Second || m.OffsetAt(time.Hour) != 1000 {
		t.Fatal("clamping")
	}
	if (TimeMap{}).TimeAt(10) != 0 || (TimeMap{}).OffsetAt(time.Second) != 0 {
		t.Fatal("invalid map should map to 0")
	}

	// With an index: the first half of the bytes covers 20% of the time.
	idx := TimeMap{Size: 1000, Duration: 100 * time.Second, Points: []IndexPoint{{500, 20 * time.Second}}}
	if got := idx.TimeAt(250); got != 10*time.Second {
		t.Fatalf("indexed TimeAt(250) = %v", got)
	}
	if got := idx.TimeAt(750); got != 60*time.Second {
		t.Fatalf("indexed TimeAt(750) = %v", got)
	}
	if got := idx.OffsetAt(60 * time.Second); got != 750 {
		t.Fatalf("indexed OffsetAt(60s) = %d", got)
	}
	// Bad points are ignored.
	bad := TimeMap{Size: 1000, Duration: 100 * time.Second, Points: []IndexPoint{{2000, time.Second}, {0, 0}}}
	if got := bad.TimeAt(500); got != 50*time.Second {
		t.Fatalf("bad-index TimeAt = %v", got)
	}

	// Round trip on a large file without overflow.
	big := TimeMap{Size: 60 << 30, Duration: 3 * time.Hour}
	off := int64(45 << 30)
	if back := big.OffsetAt(big.TimeAt(off)); back < off-1000 || back > off+1000 {
		t.Fatalf("round trip %d -> %d", off, back)
	}
}

func TestAlignTimeAndMapOffset(t *testing.T) {
	// Same cut: timestamps carry over.
	if got := AlignTime(30*time.Minute, 2*time.Hour, 2*time.Hour+30*time.Second); got != 30*time.Minute {
		t.Fatalf("same cut = %v", got)
	}
	// PAL speedup: film 2h vs PAL 2h/1.0427.
	film := 2 * time.Hour
	pal := time.Duration(float64(film) / palRatio) // film is a variable
	got := AlignTime(time.Hour, film, pal)
	hour := time.Hour
	want := time.Duration(float64(hour) / palRatio)
	if d := got - want; d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("PAL align = %v, want %v", got, want)
	}
	if back := AlignTime(got, pal, film); back-time.Hour > time.Millisecond || time.Hour-back > time.Millisecond {
		t.Fatalf("PAL back = %v", back)
	}
	// Clamped to the new duration.
	if got := AlignTime(110*time.Minute, 2*time.Hour, 100*time.Minute); got != 100*time.Minute {
		t.Fatalf("clamp = %v", got)
	}
	// Byte mapping between a 1 GB and a 4 GB release of the same 100 min cut.
	a := TimeMap{Size: 1 << 30, Duration: 100 * time.Minute}
	b := TimeMap{Size: 4 << 30, Duration: 100 * time.Minute}
	if got := MapOffset(1<<28, a, b); got != 1<<30 {
		t.Fatalf("MapOffset = %d", got)
	}
	if alignPacket(188*10+5, tsPacket) != 1880 {
		t.Fatal("alignPacket")
	}
	if alignPacket(192*10+5, tsPacketSize("mpegts", "Movie.m2ts")) != 1920 {
		t.Fatal("alignPacket m2ts")
	}
	if tsPacketSize("mpegts", "Movie.ts") != tsPacket || tsPacketSize("mpegts", "clip.MTS") != bdavPacket {
		t.Fatal("tsPacketSize")
	}
}

// ---------------------------------------------------------------------------
// Range parsing and serving

func TestParseRange(t *testing.T) {
	cases := []struct {
		h    string
		want []byteRange
		err  error
	}{
		{"bytes=0-99", []byteRange{{0, 100}}, nil},
		{"bytes=100-", []byteRange{{100, 900}}, nil},
		{"bytes=-100", []byteRange{{900, 100}}, nil},
		{"bytes=-5000", []byteRange{{0, 1000}}, nil},
		{"bytes=900-5000", []byteRange{{900, 100}}, nil},
		{"BYTES = 1-2, 4-5", []byteRange{{1, 2}, {4, 2}}, nil},
		{"bytes=1000-", nil, errRangeUnsatisfiable},
		{"bytes=-0", nil, errRangeUnsatisfiable},
		{"bytes=5-1", nil, errRangeInvalid},
		{"bytes=a-b", nil, errRangeInvalid},
		{"items=0-1", nil, errRangeInvalid},
		{"bytes=", nil, errRangeInvalid},
		{"bytes=0-1,2000-3000", []byteRange{{0, 2}}, nil},
	}
	for _, c := range cases {
		got, err := parseRange(c.h, 1000)
		if !errors.Is(err, c.err) || fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("parseRange(%q) = %v, %v; want %v, %v", c.h, got, err, c.want, c.err)
		}
	}
}

func get(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

func directPlayServer(t *testing.T, m *Manager, id string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.ServeRange(w, r, id)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServeRange(t *testing.T) {
	const size = 100_000
	h := newHarness(t, []*fakeRelease{mkv("A", size, 100*time.Second)}, []string{"A"})
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)

	resp, body, err := get(t, srv.URL, nil)
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, expected("A", 0, size)) {
		t.Fatalf("full: %d %v len=%d", resp.StatusCode, err, len(body))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("headers %v", resp.Header)
	}
	etag := resp.Header.Get("ETag")
	lastMod := resp.Header.Get("Last-Modified")
	if etag == "" || lastMod == "" {
		t.Fatal("missing validators")
	}

	check := func(name string, hdr map[string]string, status int, off, n int64, crange string) {
		t.Helper()
		resp, body, err := get(t, srv.URL, hdr)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s: status %d, want %d", name, resp.StatusCode, status)
		}
		if got := resp.Header.Get("Content-Range"); got != crange {
			t.Fatalf("%s: Content-Range %q, want %q", name, got, crange)
		}
		if status != 416 && !bytes.Equal(body, expected("A", off, n)) {
			t.Fatalf("%s: body mismatch (len %d, want %d)", name, len(body), n)
		}
	}
	check("single", map[string]string{"Range": "bytes=1000-1999"}, 206, 1000, 1000, "bytes 1000-1999/100000")
	check("open", map[string]string{"Range": "bytes=99000-"}, 206, 99000, 1000, "bytes 99000-99999/100000")
	check("suffix", map[string]string{"Range": "bytes=-500"}, 206, 99500, 500, "bytes 99500-99999/100000")
	check("unsatisfiable", map[string]string{"Range": "bytes=200000-"}, 416, 0, 0, "bytes */100000")
	check("malformed ignored", map[string]string{"Range": "bytes=x-y"}, 200, 0, size, "")
	check("if-range etag match", map[string]string{"Range": "bytes=10-19", "If-Range": etag}, 206, 10, 10, "bytes 10-19/100000")
	check("if-range etag mismatch", map[string]string{"Range": "bytes=10-19", "If-Range": `"other"`}, 200, 0, size, "")
	check("if-range weak", map[string]string{"Range": "bytes=10-19", "If-Range": "W/" + etag}, 200, 0, size, "")
	check("if-range date match", map[string]string{"Range": "bytes=10-19", "If-Range": lastMod}, 206, 10, 10, "bytes 10-19/100000")
	check("if-range date mismatch", map[string]string{"Range": "bytes=10-19", "If-Range": "Mon, 02 Jan 2006 15:04:05 GMT"}, 200, 0, size, "")

	// HEAD carries headers only.
	req, _ := http.NewRequest(http.MethodHead, srv.URL, nil)
	req.Header.Set("Range", "bytes=0-9")
	hr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	hr.Body.Close()
	if hr.StatusCode != 206 || hr.ContentLength != 10 {
		t.Fatalf("HEAD: %d %d", hr.StatusCode, hr.ContentLength)
	}

	// Multi-range.
	resp, body, err = get(t, srv.URL, map[string]string{"Range": "bytes=0-9,500-509"})
	if err != nil || resp.StatusCode != 206 {
		t.Fatalf("multi: %d %v", resp.StatusCode, err)
	}
	mt, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "multipart/byteranges" {
		t.Fatalf("multi type %q", mt)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for i, want := range []struct{ off, n int64 }{{0, 10}, {500, 10}} {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		pb, _ := io.ReadAll(p)
		if !bytes.Equal(pb, expected("A", want.off, want.n)) {
			t.Fatalf("part %d mismatch", i)
		}
		if cr := p.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes %d-%d/%d", want.off, want.off+want.n-1, size) {
			t.Fatalf("part %d range %q", i, cr)
		}
	}

	// Method and unknown session.
	resp2, _ := http.Post(srv.URL, "text/plain", nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", resp2.StatusCode)
	}
	rec := httptest.NewRecorder()
	h.m.ServeRange(rec, httptest.NewRequest("GET", "/", nil), "nope")
	if rec.Code != 404 {
		t.Fatalf("unknown session: %d", rec.Code)
	}

	// A Range at 85% of a large file must not walk 0→offset. Probe may
	// have touched the header and cues; the request itself stays at the
	// requested offset.
	t.Run("far-offset", func(t *testing.T) {
		const big = 8_000_000
		rel := mkv("A", big, 100*time.Second)
		rel.ReadDelay = 80 * time.Millisecond
		h := newHarness(t, []*fakeRelease{rel}, []string{"A"})
		ps := h.prepare(owner, movieKey, "c1")
		before := map[*fakeSource]int{}
		for _, src := range h.engine.sources {
			before[src] = len(src.readSpans())
		}
		srv := directPlayServer(t, h.m, ps.ID)
		off := int64(big) * 85 / 100
		started := time.Now()
		resp, body, err := get(t, srv.URL, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", off, off+4095)})
		if err != nil || resp.StatusCode != 206 || !bytes.Equal(body, expected("A", off, 4096)) {
			t.Fatalf("far range: %d %v len=%d", resp.StatusCode, err, len(body))
		}
		if d := time.Since(started); d > time.Second {
			t.Fatalf("85%% range took %s (sequential fill?)", d)
		}
		for _, src := range h.engine.sources {
			for _, span := range src.readSpans()[before[src]:] {
				// A few header bytes for the time map are fine; walking
				// the file toward the offset is not.
				if span[0] < off-int64(h.m.opts.ChunkSize) && span[1]-span[0] > headerLen {
					t.Fatalf("ServeRange at %d also read [%d,%d)", off, span[0], span[1])
				}
			}
		}
	})

	// Status details.
	st, _ := h.m.Session(ps.ID)
	if st.Decision != core.DecisionDirectPlay || st.BytesServed < 2*size || st.TitleName != "The Signal (1999)" ||
		st.Username != "owner" || st.Resolution.Release.Name != "Release.A" || st.Client.ClientID != "c1" {
		t.Fatalf("status = %+v", st)
	}
}

func TestRateMeter(t *testing.T) {
	var r rateMeter
	base := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		r.add(1000, base.Add(time.Duration(i)*time.Second))
	}
	if got := r.rate(base.Add(5 * time.Second)); got != 1000 {
		t.Fatalf("rate = %d", got)
	}
	if got := r.rate(base.Add(time.Minute)); got != 0 {
		t.Fatalf("stale rate = %d", got)
	}
}

// ---------------------------------------------------------------------------
// Prepare: limits, reuse, singleflight

func TestStreamLimits(t *testing.T) {
	rels := []*fakeRelease{mkv("A", 10000, time.Minute)}
	t.Run("per user", func(t *testing.T) {
		h := newHarness(t, rels, []string{"A"})
		u := *friend
		u.StreamLimit = 1
		h.prepare(&u, movieKey, "c1")
		// Reuse of the same session does not count.
		h.prepare(&u, movieKey, "c1")
		_, err := h.m.Prepare(context.Background(), &u, movie2, defaultCaps("c2"))
		if !errors.Is(err, core.ErrStreamLimit) {
			t.Fatalf("err = %v", err)
		}
		// The same client switching titles replaces its session.
		ps := h.prepare(&u, movie2, "c1")
		if n := len(h.m.Sessions()); n != 1 || h.m.Sessions()[0].ID != ps.ID {
			t.Fatalf("sessions = %d", n)
		}
		// Another user is unaffected.
		h.prepare(owner, movieKey, "c9")
	})
	t.Run("server default", func(t *testing.T) {
		h := newHarness(t, rels, []string{"A"}, func(h *harness, o *Options) { h.cfg.Server.DefaultStreamLimit = 2 })
		h.prepare(friend, movieKey, "c1")
		h.prepare(friend, movie2, "c2")
		if _, err := h.m.Prepare(context.Background(), friend, movie3, defaultCaps("c3")); !errors.Is(err, core.ErrStreamLimit) {
			t.Fatalf("err = %v", err)
		}
		// Unlimited user override.
		u := *friend
		u.StreamLimit = -1
		h.prepare(&u, movie3, "c3")
	})
	t.Run("max streams", func(t *testing.T) {
		h := newHarness(t, rels, []string{"A"}, func(h *harness, o *Options) { h.cfg.Server.MaxStreams = 1 })
		h.prepare(owner, movieKey, "c1")
		if _, err := h.m.Prepare(context.Background(), friend, movieKey, defaultCaps("c2")); !errors.Is(err, core.ErrStreamLimit) {
			t.Fatalf("err = %v", err)
		}
		// A config change lifts it.
		cfg := h.cfg
		cfg.Server.MaxStreams = 2
		h.m.SetConfig(cfg)
		h.prepare(friend, movieKey, "c2")
	})
	t.Run("derived from connections", func(t *testing.T) {
		cfg := config.Defaults(t.TempDir())
		cfg.Usenet.Providers = []config.UsenetProvider{
			{Enabled: true, Connections: 20},
			{Enabled: true, Connections: 30, Backfill: true},
			{Enabled: false, Connections: 50},
			{Enabled: true, Connections: 4},
		}
		if got := ServerStreamLimit(cfg, 8); got != 3 {
			t.Fatalf("derived = %d", got)
		}
		cfg.Usenet.Providers = cfg.Usenet.Providers[3:]
		if got := ServerStreamLimit(cfg, 8); got != 1 {
			t.Fatalf("derived min = %d", got)
		}
		cfg.Usenet.Providers = nil
		if got := ServerStreamLimit(cfg, 8); got != 0 {
			t.Fatalf("no providers = %d", got)
		}
		cfg.Server.MaxStreams = 5
		if got := ServerStreamLimit(cfg, 8); got != 5 {
			t.Fatalf("explicit = %d", got)
		}
	})
}

func TestPrepareSingleflight(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	h.resolver.delay = 100 * time.Millisecond
	var wg sync.WaitGroup
	ids := make([]string, 20)
	errs := make([]error, 20)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ps, err := h.m.Prepare(context.Background(), owner, movieKey, defaultCaps("c1"))
			errs[i] = err
			if err == nil {
				ids[i] = ps.ID
			}
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("caller %d: %q %v (want %q)", i, ids[i], errs[i], ids[0])
		}
	}
	if n := h.resolver.callCount(); n != 1 {
		t.Fatalf("Resolve called %d times", n)
	}
	if n := h.engine.openCount(); n != 1 {
		t.Fatalf("Open called %d times", n)
	}
}

func TestPrepareWaiterCancel(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	h.resolver.delay = 200 * time.Millisecond
	h.resolver.started = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := h.m.Prepare(ctx, owner, movieKey, defaultCaps("c1"))
		errc <- err
	}()
	<-h.resolver.started
	done := make(chan *core.PlaySession, 1)
	go func() {
		ps, err := h.m.Prepare(context.Background(), owner, movieKey, defaultCaps("c1"))
		if err != nil {
			t.Error(err)
		}
		done <- ps
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter err = %v", err)
	}
	if ps := <-done; ps == nil {
		t.Fatal("second waiter got no session")
	}
	if n := h.resolver.callCount(); n != 1 {
		t.Fatalf("Resolve called %d times", n)
	}
}

func TestPrepareErrors(t *testing.T) {
	h := newHarness(t, nil, nil)
	if _, err := h.m.Prepare(context.Background(), owner, movieKey, defaultCaps("c")); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.m.Prepare(context.Background(), nil, movieKey, defaultCaps("c")); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("nil user err = %v", err)
	}
	if _, err := h.m.Prepare(context.Background(), owner, core.MovieKey(1), defaultCaps("c")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown title err = %v", err)
	}
	// The pending reservation is released after failures.
	h.m.mu.Lock()
	pending := h.m.pendingN
	h.m.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d", pending)
	}
}

func TestPrepareSkipsBrokenRelease(t *testing.T) {
	broken := mkv("A", 10000, time.Minute)
	broken.setFail(0, 10000, nil) // header missing: the probe fails
	h := newHarness(t, []*fakeRelease{broken, mkv("B", 20000, time.Minute)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "B" {
		t.Fatalf("release = %s", ps.Resolution.Release.ID)
	}
	if !h.resolver.isBad("A") {
		t.Fatal("A not marked bad")
	}
	if ex := h.resolver.excludes[len(h.resolver.excludes)-1]; len(ex) != 1 || ex[0] != "A" {
		t.Fatalf("exclude = %v", ex)
	}
	// The broken generation's source was closed.
	if !h.engine.sources[0].closed.Load() {
		t.Fatal("broken source left open")
	}
}

func TestPrepareProbeFailureUsesEstimate(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	h.tr.failAll = true
	ps := h.prepare(owner, movieKey, "c1")
	if !ps.Info.Estimated || ps.Info.Size != 10000 || ps.Info.Duration != 136*time.Minute {
		t.Fatalf("info = %+v", ps.Info)
	}
}

// ---------------------------------------------------------------------------
// Failover

func TestFailoverMidResponseEndsMKV(t *testing.T) {
	const sizeA, sizeB = 200_000, 200_000
	a := mkv("A", sizeA, 100*time.Second)
	a.setFail(120_000, 130_000, nil)
	h := newHarness(t, []*fakeRelease{a, mkv("B", sizeB, 100*time.Second)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)

	resp, body, err := get(t, srv.URL, nil)
	if resp.StatusCode != 200 || err == nil {
		t.Fatalf("want a truncated 200, got %d err=%v len=%d", resp.StatusCode, err, len(body))
	}
	if int64(len(body)) != a.FailFrom || !bytes.Equal(body, expected("A", 0, a.FailFrom)) {
		t.Fatalf("got %d bytes before the cut, want %d intact", len(body), a.FailFrom)
	}
	if !h.resolver.isBad("A") {
		t.Fatal("A not marked bad")
	}
	st, _ := h.m.Session(ps.ID)
	if st.Resolution.Release.ID != "B" || st.Failovers != 1 {
		t.Fatalf("after failover: %s failovers=%d", st.Resolution.Release.ID, st.Failovers)
	}
	if !h.cache.pinned("B", PinReasonLive) || h.cache.pinned("A", PinReasonLive) {
		t.Fatalf("pins = %v", h.cache.pins)
	}

	// The client re-requests from where it stopped; virtual coordinates
	// (A's size) remain valid for the same-size repost B.
	resp, body, err = get(t, srv.URL, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", a.FailFrom, a.FailFrom+999)})
	if err != nil || resp.StatusCode != 206 {
		t.Fatalf("re-request: %d %v", resp.StatusCode, err)
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes %d-%d/%d", a.FailFrom, a.FailFrom+999, sizeA) {
		t.Fatalf("Content-Range %q", cr)
	}
	mapped := a.FailFrom * sizeB / sizeA // same duration: linear
	if !bytes.Equal(body, expected("B", mapped, 1000)) {
		t.Fatal("re-request not served from B at the mapped offset")
	}
}

func TestDirectPlayRejectsDifferentSizeReplacement(t *testing.T) {
	// No exact-size repost exists, so the mismatched release must never be
	// spliced into A's byte coordinates: the response ends at the failure
	// point, the session swaps to B, and the next decision must be a
	// remux (RequiresRemux).
	a := mkv("A", 200000, 100*time.Second)
	a.setFail(120000, 130000, nil)
	h := newHarness(t, []*fakeRelease{a, mkv("B", 400000, 100*time.Second)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)
	_, body, err := get(t, srv.URL, nil)
	if err == nil || len(body) != int(a.FailFrom) {
		t.Fatalf("expected source to stop safely: bytes=%d err=%v", len(body), err)
	}
	st, _ := h.m.Session(ps.ID)
	if st.Resolution.Release.ID != "B" || st.Failovers != 1 {
		t.Fatalf("session did not swap to the healthy release: %+v", st)
	}
	if !st.RequiresRemux || !h.m.RequiresRemux(ps.ID) {
		t.Fatalf("remux switch not flagged: %+v", st)
	}
	if !h.cache.pinned("B", PinReasonLive) {
		t.Fatal("swapped candidate was not pinned")
	}
	if h.resolver.isBad("B") {
		t.Fatal("healthy candidate marked bad")
	}
	// A native client can retry the old part URL without issuing another
	// decision. Never map A's ranges into B or let them trigger more swaps.
	for _, remuxStarted := range []bool{false, true} {
		if remuxStarted {
			h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux})
		}
		resp, body, err := get(t, srv.URL, map[string]string{"Range": "bytes=0-999"})
		if err != nil || resp.StatusCode != http.StatusConflict || resp.Header.Get("Content-Range") != "" {
			t.Fatalf("stale part after remux=%v: status=%d err=%v body=%q", remuxStarted, resp.StatusCode, err, body)
		}
		var out bytes.Buffer
		if err := h.m.streamClient(t.Context(), &out, h.m.lookup(ps.ID), 0, 1000); !errors.Is(err, errEndResponse) || out.Len() != 0 {
			t.Fatalf("stale stream copied replacement bytes: n=%d err=%v", out.Len(), err)
		}
	}
}

func TestFailoverMidResponseSplicesTS(t *testing.T) {
	const sizeA, sizeB = 188 * 1000, 188 * 3000
	a := &fakeRelease{ID: "A", Name: "A.ts", Size: sizeA, Duration: 50 * time.Second, FailFrom: 188*500 + 17, FailTo: 188 * 600}
	b := &fakeRelease{ID: "B", Name: "B.ts", Size: sizeB, Duration: 50 * time.Second}
	h := newHarness(t, []*fakeRelease{a, b}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)

	resp, body, err := get(t, srv.URL, nil)
	if err != nil || resp.StatusCode != 200 || len(body) != sizeA {
		t.Fatalf("splice: %d %v len=%d", resp.StatusCode, err, len(body))
	}
	if !bytes.Equal(body[:a.FailFrom], expected("A", 0, a.FailFrom)) {
		t.Fatal("prefix not from A")
	}
	newOff := alignPacket(a.FailFrom*sizeB/sizeA, tsPacket)
	rest := int64(sizeA) - a.FailFrom
	if !bytes.Equal(body[a.FailFrom:], expected("B", newOff, rest)) {
		t.Fatal("suffix not spliced from B at the aligned mapped offset")
	}
}

func TestFailoverMidResponseSplicesBDAVOnPacketBoundary(t *testing.T) {
	const sizeA, sizeB = 192 * 1000, 192 * 3000
	a := &fakeRelease{ID: "A", Name: "A.m2ts", Size: sizeA, Duration: 50 * time.Second, FailFrom: 192*500 + 17, FailTo: 192 * 600}
	b := &fakeRelease{ID: "B", Name: "B.m2ts", Size: sizeB, Duration: 50 * time.Second}
	h := newHarness(t, []*fakeRelease{a, b}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)

	resp, body, err := get(t, srv.URL, nil)
	if err != nil || resp.StatusCode != 200 || len(body) != sizeA {
		t.Fatalf("splice: %d %v len=%d", resp.StatusCode, err, len(body))
	}
	newOff := alignPacket(a.FailFrom*sizeB/sizeA, bdavPacket)
	if newOff%192 != 0 || newOff%188 == 0 {
		t.Fatalf("test offset %d does not distinguish packet sizes", newOff)
	}
	if !bytes.Equal(body[a.FailFrom:], expected("B", newOff, int64(sizeA)-a.FailFrom)) {
		t.Fatal("suffix not spliced from B at a 192-byte packet boundary")
	}
}

func TestFailoverMidResponseDoesNotSpliceAcrossPacketSizes(t *testing.T) {
	const sizeA, sizeB = 188 * 1000, 192 * 3000
	a := &fakeRelease{ID: "A", Name: "A.ts", Size: sizeA, Duration: 50 * time.Second, FailFrom: 188*500 + 17, FailTo: 188 * 600}
	b := &fakeRelease{ID: "B", Name: "B.m2ts", Size: sizeB, Duration: 50 * time.Second}
	h := newHarness(t, []*fakeRelease{a, b}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)

	_, body, _ := get(t, srv.URL, nil)
	if int64(len(body)) != a.FailFrom || !bytes.Equal(body, expected("A", 0, a.FailFrom)) {
		t.Fatalf("response continued past the failure into another packet size: len=%d", len(body))
	}
}

func TestFailoverHLS(t *testing.T) {
	a := mkv("A", 100_000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a, mkv("B", 300_000, 100*time.Second), mkv("C", 50_000, 100*time.Second)}, []string{"A", "B", "C"})
	ps := h.prepare(owner, movieKey, "c1")
	hls := h.tr.addHLS(ps.ID)
	oldURL := h.m.InputURL(ps.ID)

	ps2, err := h.m.Failover(context.Background(), ps.ID, 42*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ps2.Resolution.Release.ID != "B" || ps2.Info.Size != 300_000 {
		t.Fatalf("after failover: %+v", ps2.Resolution.Release)
	}
	sw := <-hls.swapC
	newURL := h.m.InputURL(ps.ID)
	if sw.URL != newURL || sw.At != 42*time.Second || sw.Info.Size != 300_000 || newURL == oldURL {
		t.Fatalf("swap = %+v (new url %s, old %s)", sw, newURL, oldURL)
	}
	if resp, _, _ := get(t, oldURL, nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("old url status %d", resp.StatusCode)
	}
	resp, body, err := get(t, newURL, map[string]string{"Range": "bytes=1000-1999"})
	if err != nil || resp.StatusCode != 206 || !bytes.Equal(body, expected("B", 1000, 1000)) {
		t.Fatalf("new url: %d %v", resp.StatusCode, err)
	}
	if h.resolver.isBad("A") {
		t.Fatal("manual failover without permanent evidence blacklisted A")
	}
	// The retired source is closed.
	if !h.engine.sources[0].closed.Load() {
		t.Fatal("A's source not closed")
	}
	// A second failover excludes both earlier releases.
	ps3, err := h.m.Failover(context.Background(), ps.ID, 50*time.Second)
	if err != nil || ps3.Resolution.Release.ID != "C" {
		t.Fatalf("second failover: %v %v", err, ps3)
	}
	if ex := h.resolver.excludes[len(h.resolver.excludes)-1]; strings.Join(ex, ",") != "A,B" {
		t.Fatalf("exclude = %v", ex)
	}
	// Nothing left.
	if _, err := h.m.Failover(context.Background(), ps.ID, time.Minute); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("exhausted err = %v", err)
	}
	if _, err := h.m.Failover(context.Background(), "nope", 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown err = %v", err)
	}
}

func TestFailoverUsesAlternatesWithFetchNZB(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 100_000, time.Minute), mkv("B", 100_000, time.Minute)}, []string{"A", "B"})
	var fetched []string
	h.m.deps.FetchNZB = func(ctx context.Context, r core.Release) ([]byte, error) {
		fetched = append(fetched, r.ID)
		return []byte("nzb-" + r.ID), nil
	}
	ps := h.prepare(owner, movieKey, "c1")
	calls := h.resolver.callCount()
	ps2, err := h.m.Failover(context.Background(), ps.ID, 10*time.Second)
	if err != nil || ps2.Resolution.Release.ID != "B" {
		t.Fatalf("failover: %v", err)
	}
	if h.resolver.callCount() != calls || len(fetched) != 1 || fetched[0] != "B" {
		t.Fatalf("resolver calls %d->%d fetched %v", calls, h.resolver.callCount(), fetched)
	}
}

func TestLoopbackReadFailureTriggersHLSFailover(t *testing.T) {
	a := mkv("A", 100_000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a, mkv("B", 100_000, 100*time.Second)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	hls := h.tr.addHLS(ps.ID)
	a.setFail(50_000, 60_000, nil) // goes missing after the probe

	resp, _, err := get(t, h.m.InputURL(ps.ID), map[string]string{"Range": "bytes=40000-"})
	if resp.StatusCode != 206 || err == nil {
		t.Fatalf("want truncated 206, got %d %v", resp.StatusCode, err)
	}
	select {
	case sw := <-hls.swapC:
		if sw.At != 50*time.Second || !strings.Contains(sw.URL, "/src/"+ps.ID+"/") {
			t.Fatalf("swap = %+v", sw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no background failover")
	}
	st, _ := h.m.Session(ps.ID)
	if st.Resolution.Release.ID != "B" {
		t.Fatalf("release = %s", st.Resolution.Release.ID)
	}
}

func TestLoopbackServer(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 100_000, time.Minute)}, []string{"A"})
	ps := h.prepare(owner, movieKey, "c1")
	u := h.m.InputURL(ps.ID)
	if !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.Contains(u, ps.ID) {
		t.Fatalf("InputURL = %s", u)
	}
	resp, body, err := get(t, u, map[string]string{"Range": "bytes=-100"})
	if err != nil || resp.StatusCode != 206 || !bytes.Equal(body, expected("A", 99_900, 100)) {
		t.Fatalf("loopback suffix: %d %v", resp.StatusCode, err)
	}
	// Guessing the session id alone is not enough.
	base := u[:strings.LastIndex(u, "/")]
	if resp, _, _ := get(t, base+"/0000", nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("guessed token: %d", resp.StatusCode)
	}
	if h.m.InputURL("nope") != "" {
		t.Fatal("InputURL for unknown session")
	}
	h.m.Stop(ps.ID)
	if resp, _, _ := get(t, u, nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("after stop: %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Expiry, timeline, stop

func TestIdleExpiryAndTimeline(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute)}, []string{"A"},
		func(h *harness, o *Options) { o.IdleTimeout = 10 * time.Minute; o.StoppedGrace = time.Minute })
	ps := h.prepare(owner, movieKey, "c1")
	h.clock.Advance(9 * time.Minute)
	h.m.expire()
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("expired too early")
	}
	h.m.Timeline(owner.ID, movieKey, "playing")
	h.clock.Advance(9 * time.Minute)
	h.m.expire()
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("timeline did not keep it alive")
	}
	h.clock.Advance(2 * time.Minute)
	h.m.expire()
	if _, ok := h.m.Session(ps.ID); ok {
		t.Fatal("idle session not expired")
	}
	// The retired generation closes its source asynchronously.
	eventually(t, "source closed", func() bool { return h.engine.sources[0].closed.Load() })
	if h.cache.pinned("A", PinReasonLive) {
		t.Fatal("live pin left")
	}

	// Stopped: ends after the grace unless the client comes back.
	ps = h.prepare(owner, movieKey, "c1")
	h.m.Timeline(owner.ID, movieKey, "stopped")
	h.clock.Advance(30 * time.Second)
	h.m.expire()
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("stopped session ended before grace")
	}
	h.clock.Advance(31 * time.Second)
	h.m.expire()
	if _, ok := h.m.Session(ps.ID); ok {
		t.Fatal("stopped session not ended")
	}
}

func TestStopStopsHLS(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute)}, []string{"A"})
	ps := h.prepare(owner, movieKey, "c1")
	h.tr.addHLS(ps.ID)
	h.m.Stop(ps.ID)
	h.m.Stop(ps.ID) // idempotent
	if len(h.tr.stopped) != 1 || h.tr.stopped[0] != ps.ID {
		t.Fatalf("transcoder stops = %v", h.tr.stopped)
	}
	if _, ok := h.m.SessionFor(owner.ID, "c1", movieKey); ok {
		t.Fatal("SessionFor after stop")
	}
}

// ---------------------------------------------------------------------------
// Cache protection and resume

func TestPinsAndResume(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute), mkv("B", 10_000, time.Minute)}, []string{"A", "B"},
		func(h *harness, o *Options) { o.ResumeRetention = 24 * time.Hour })
	ctx := context.Background()
	ps := h.prepare(owner, movieKey, "c1")
	if !h.cache.pinned("A", PinReasonLive) {
		t.Fatal("live release not pinned")
	}
	if got, ok := h.m.SessionFor(owner.ID, "c1", movieKey); !ok || got.ID != ps.ID {
		t.Fatal("SessionFor")
	}

	// The resume record and NZB are stored.
	rp, err := h.m.ResumePoint(ctx, owner.ID, movieKey)
	if err != nil || rp == nil || rp.ReleaseID() != "A" || string(rp.NZB) != "nzb-A" || rp.Size != 10_000 || rp.Duration != time.Minute || rp.Profile != "1080p" {
		t.Fatalf("resume = %+v %v", rp, err)
	}

	// In progress for the owner: pinned beyond the session.
	h.state.set(owner.ID, movieKey, 20*time.Second)
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	h.m.Stop(ps.ID)
	if h.cache.pinned("A", PinReasonLive) || !h.cache.pinned("A", PinReasonInProgress) {
		t.Fatalf("pins after stop = %v", h.cache.pins)
	}

	// Resuming reuses the stored release without the resolver.
	calls := h.resolver.callCount()
	ps2 := h.prepare(owner, movieKey, "c2")
	if ps2.Resolution.Release.ID != "A" || h.resolver.callCount() != calls {
		t.Fatalf("resume used %s, resolver calls %d->%d", ps2.Resolution.Release.ID, calls, h.resolver.callCount())
	}
	h.m.Stop(ps2.ID)

	// A client with another profile resolves afresh.
	caps := defaultCaps("tv")
	caps.MaxHeight = 2160
	if _, err := h.m.Prepare(ctx, owner, movieKey, caps); err != nil {
		t.Fatal(err)
	}
	if h.resolver.callCount() != calls+1 {
		t.Fatal("different profile did not resolve")
	}
	for _, s := range h.m.Sessions() {
		h.m.Stop(s.ID)
	}

	// Finished: unpinned; after retention the record and NZB are dropped.
	h.state.set(owner.ID, movieKey, 0)
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if h.cache.pinned("A", PinReasonInProgress) {
		t.Fatal("finished title still pinned")
	}
	if rp, _ := h.m.ResumePoint(ctx, owner.ID, movieKey); rp == nil {
		t.Fatal("record dropped before retention")
	}
	h.clock.Advance(25 * time.Hour)
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if rp, _ := h.m.ResumePoint(ctx, owner.ID, movieKey); rp != nil {
		t.Fatal("record kept past retention")
	}
}

func TestReconcileUnpinsStaleOnFirstPass(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute)}, []string{"A"})
	ctx := context.Background()
	ps := h.prepare(owner, movieKey, "c1")
	h.m.Stop(ps.ID)
	// Simulate pins left by a previous process.
	h.cache.Pin("A", PinReasonInProgress)
	h.m.pinMu.Lock()
	h.m.pinsSwept = false
	h.m.pinMu.Unlock()
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if h.cache.pinned("A", PinReasonInProgress) {
		t.Fatal("stale pin kept")
	}
}

func TestRememberHint(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute), mkv("N", 10_000, time.Minute)}, []string{"A"})
	ctx := context.Background()
	res := &core.Resolution{Release: core.Release{ID: "N", Name: "Next"}, NZB: []byte("nzb-N")}
	if err := h.m.Remember(ctx, owner, epKey, defaultCaps("c1"), res); err != nil {
		t.Fatal(err)
	}
	// Remember does not override an existing record for the profile.
	other := &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("nzb-A")}
	if err := h.m.Remember(ctx, owner, epKey, defaultCaps("c1"), other); err != nil {
		t.Fatal(err)
	}
	ps := h.prepare(owner, epKey, "c1")
	if ps.Resolution.Release.ID != "N" || h.resolver.callCount() != 0 {
		t.Fatalf("prepare used %s (resolver calls %d)", ps.Resolution.Release.ID, h.resolver.callCount())
	}
	if ps.TitleName != "Boundary Bay - S01E01 - Pilot" {
		t.Fatalf("title = %q", ps.TitleName)
	}
	caps, live, ok := h.m.ClientCaps(owner.ID, epKey)
	if !ok || !live || caps.ClientID != "c1" {
		t.Fatalf("ClientCaps = %v %v %v", caps, live, ok)
	}
	if _, live, ok := h.m.ClientCaps(owner.ID, movieKey); live || !ok {
		t.Fatalf("fallback caps live=%v ok=%v", live, ok)
	}
}

func TestConcurrentServeAndFailover(t *testing.T) {
	a := mkv("A", 500_000, 100*time.Second)
	a.setFail(250_000, 260_000, nil)
	h := newHarness(t, []*fakeRelease{a, mkv("B", 500_000, 100*time.Second)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "c1")
	srv := directPlayServer(t, h.m, ps.ID)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", srv.URL, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	st, _ := h.m.Session(ps.ID)
	if st.Failovers != 1 || st.Resolution.Release.ID != "B" {
		t.Fatalf("failovers = %d release %s", st.Failovers, st.Resolution.Release.ID)
	}
	if len(h.m.Sessions()) != 1 {
		t.Fatal("sessions")
	}
}

func TestReadFailuresRecoverWithoutPoisoningReleases(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		bad  bool
	}{
		{"corrupt", core.ErrArticleCorrupt, true},
		{"timeout", context.DeadlineExceeded, false},
		{"provider-backoff", core.ErrSourceUnavailable, false},
		{"truncated", io.ErrUnexpectedEOF, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := mkv("A", 100000, 100*time.Second), mkv("B", 100000, 100*time.Second)
			h := newHarness(t, []*fakeRelease{a, b}, []string{"A", "B"})
			ps := h.prepare(owner, movieKey, "reader")
			a.setFail(5000, 10000, tc.err)
			srv := directPlayServer(t, h.m, ps.ID)
			resp, body, err := get(t, srv.URL, map[string]string{"Range": "bytes=6000-6999"})
			if err != nil || resp.StatusCode != 206 || !bytes.Equal(body, expected("B", 6000, 1000)) {
				t.Fatalf("failed to recover read: %v %d", err, resp.StatusCode)
			}
			if bad := h.resolver.isBad("A"); bad != tc.bad {
				t.Fatalf("release blacklisted=%v want %v", bad, tc.bad)
			}
		})
	}
}

func TestPausedPlaybackSurvivesPhoneCall(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Hour)}, []string{"A"},
		func(h *harness, o *Options) { o.IdleTimeout = 2 * time.Minute })
	ps := h.prepare(owner, movieKey, "c1")
	h.m.PlaybackProgress(owner.ID, "c1", movieKey, time.Minute, "paused")
	if !h.cache.pinned("A", PinReasonLive) {
		t.Fatal("paused session lost live cache protection")
	}
	sessions := h.m.Sessions()
	if len(sessions) != 1 || sessions[0].Resolution == nil || sessions[0].Resolution.Release.ID != "A" {
		t.Fatalf("paused session missing from owner protection: %+v", sessions)
	}
	h.clock.Advance(5 * time.Minute)
	h.m.expire()
	if _, ok := h.m.Session(ps.ID); !ok {
		t.Fatal("paused source expired")
	}
	h.m.PlaybackProgress(owner.ID, "c1", movieKey, time.Minute, "playing")
	h.clock.Advance(3 * time.Minute)
	h.m.expire()
	if _, ok := h.m.Session(ps.ID); ok {
		t.Fatal("resumed abandoned source retained")
	}
}

func TestStreamLimitsReleaseStoppedAndIdleSessions(t *testing.T) {
	rels := []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}
	t.Run("stopped or surface-prepared releases slot immediately", func(t *testing.T) {
		h := newHarness(t, rels, []string{"A", "B"}, func(h *harness, o *Options) {
			h.cfg.Server.MaxStreams = 2
			o.StoppedGrace = time.Minute
			o.IdleTimeout = 5 * time.Minute
		})
		ctx := context.Background()
		infuse1 := core.ClientCaps{ClientID: "infuse-1", Protocol: "jellyfin"}
		infuse2 := core.ClientCaps{ClientID: "infuse-2", Protocol: "jellyfin"}
		silo := core.ClientCaps{ClientID: "silo-1", Protocol: "silo"}

		ps1, err := h.m.PrepareSurface(ctx, owner, movieKey, infuse1, "A", true)
		if err != nil {
			t.Fatal(err)
		}
		h.m.MarkSurfaceOpened(ps1.ID)

		ps2, err := h.m.PrepareSurface(ctx, owner, movie2, infuse2, "A", true)
		if err != nil {
			t.Fatal(err)
		}
		h.m.MarkSurfaceOpened(ps2.ID)

		// While both streams are active within the activity window, a third stream hits the cap.
		if _, err := h.m.PrepareSurface(ctx, owner, movie3, silo, "A", true); !errors.Is(err, core.ErrStreamLimit) {
			t.Fatalf("expected ErrStreamLimit while 2 streams are active, got %v", err)
		}

		// When Infuse stops while a detail plan still holds the surface prepared, it must stop counting immediately.
		h.m.MarkSurfacePrepared(ps1.ID)
		if h.m.SessionActive(ps1.ID) {
			t.Fatal("surface-prepared session after stop still reported active")
		}
		ps3, err := h.m.PrepareSurface(ctx, owner, movie3, silo, "A", true)
		if err != nil {
			t.Fatalf("Silo blocked after Infuse stopped: %v", err)
		}
		h.m.Stop(ps3.ID)

		// Re-open ps1, then stop via PlaybackProgress (item-level stop).
		h.m.MarkSurfaceOpened(ps1.ID)
		if _, err := h.m.PrepareSurface(ctx, owner, movie3, silo, "A", true); !errors.Is(err, core.ErrStreamLimit) {
			t.Fatalf("expected ErrStreamLimit after re-opening ps1, got %v", err)
		}
		h.m.PlaybackProgress(owner.ID, "infuse-1", movieKey, 10*time.Second, "stopped")
		if h.m.SessionActive(ps1.ID) {
			t.Fatal("PlaybackProgress(stopped) session still reported active")
		}
		ps4, err := h.m.PrepareSurface(ctx, owner, movie3, silo, "A", true)
		if err != nil {
			t.Fatalf("Silo blocked after PlaybackProgress(stopped): %v", err)
		}
		h.m.Stop(ps4.ID)
	})

	t.Run("idle sessions without reads stop counting after activity timeout", func(t *testing.T) {
		h := newHarness(t, rels, []string{"A", "B"}, func(h *harness, o *Options) {
			h.cfg.Server.MaxStreams = 2
			o.StreamActivityTimeout = 12 * time.Second
			o.IdleTimeout = 5 * time.Minute
		})
		ctx := context.Background()
		infuse1 := core.ClientCaps{ClientID: "infuse-sintel", Protocol: "jellyfin"}
		infuse2 := core.ClientCaps{ClientID: "infuse-paywall", Protocol: "jellyfin"}
		silo := core.ClientCaps{ClientID: "silo-web", Protocol: "silo"}

		// Infuse 1 plays Sintel and stops reading without sending Stop; Infuse 2 hits AV1 paywall after PrepareSurface.
		ps1, err := h.m.PrepareSurface(ctx, owner, movieKey, infuse1, "A", true)
		if err != nil {
			t.Fatal(err)
		}
		h.m.MarkSurfaceOpened(ps1.ID)
		ps2, err := h.m.PrepareSurface(ctx, owner, movie2, infuse2, "A", true)
		if err != nil {
			t.Fatal(err)
		}

		// At t=5s, both are within the 12s window so Silo is blocked.
		h.clock.Advance(5 * time.Second)
		if _, err := h.m.PrepareSurface(ctx, owner, movie3, silo, "A", true); !errors.Is(err, core.ErrStreamLimit) {
			t.Fatalf("expected ErrStreamLimit at 5s, got %v", err)
		}

		// At t=13s (>12s with no ServeRange or HLS segment reads), both idle sessions stop counting.
		h.clock.Advance(8 * time.Second)
		if h.m.SessionActive(ps1.ID) || h.m.SessionActive(ps2.ID) {
			t.Fatalf("idle sessions still active after 13s: ps1=%v ps2=%v", h.m.SessionActive(ps1.ID), h.m.SessionActive(ps2.ID))
		}
		ps3, err := h.m.PrepareSurface(ctx, owner, movie3, silo, "A", true)
		if err != nil {
			t.Fatalf("Silo blocked by stale idle sessions at 13s: %v", err)
		}
		h.m.MarkSurfaceOpened(ps3.ID)

		// If ps1 performs a real range read and ps3 is active, a 4th client is blocked.
		srv1 := directPlayServer(t, h.m, ps1.ID)
		resp, _, err := get(t, srv1.URL, map[string]string{"Range": "bytes=0-63"})
		if err != nil || resp.StatusCode != 206 {
			t.Fatalf("range read: %d %v", resp.StatusCode, err)
		}
		if !h.m.SessionActive(ps1.ID) {
			t.Fatal("ps1 not active immediately after range read")
		}
		if _, err := h.m.PrepareSurface(ctx, owner, epKey, core.ClientCaps{ClientID: "other", Protocol: "silo"}, "A", true); !errors.Is(err, core.ErrStreamLimit) {
			t.Fatalf("expected ErrStreamLimit when 2 streams are genuinely active, got %v", err)
		}
	})
}
