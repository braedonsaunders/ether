package debrid

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// patterned is a seekable file of size bytes whose byte i is i%251.
type patterned struct{ size, off int64 }

func (p *patterned) Read(b []byte) (int, error) {
	if p.off >= p.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(b)), p.size-p.off))
	for i := 0; i < n; i++ {
		b[i] = byte((p.off + int64(i)) % 251)
	}
	p.off += int64(n)
	return n, nil
}

func (p *patterned) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		p.off = off
	case io.SeekCurrent:
		p.off += off
	case io.SeekEnd:
		p.off = p.size + off
	}
	return p.off, nil
}

const (
	directSize = 40 << 20
	addonKey   = "ADDONKEY42"
	publicIP   = "93.184.216.34"
)

// directRig is an addon (on loopback, the trusted origin) whose resolver
// redirects to a CDN that the fake guard maps onto a public address.
type directRig struct {
	addon, cdn               *httptest.Server
	resolves, cdnHits        atomic.Int64
	mu                       sync.Mutex
	addonHeaders, cdnHeaders []string
	cdnFail                  atomic.Int64 // answer this many CDN requests with 403
	svc                      *Service
}

func newDirectRig(t *testing.T, cfg config.Debrid) *directRig {
	t.Helper()
	r := &directRig{}
	serve := func(w http.ResponseWriter, req *http.Request, size int64, name string) {
		w.Header().Set("Content-Type", "video/x-matroska")
		http.ServeContent(w, req, name, time.Time{}, &patterned{size: size})
	}
	r.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.cdnHits.Add(1)
		r.mu.Lock()
		r.cdnHeaders = append(r.cdnHeaders, req.Header.Get("X-Addon-Key"))
		r.mu.Unlock()
		if r.cdnFail.Load() > 0 {
			r.cdnFail.Add(-1)
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		serve(w, req, directSize, "Movie.2014.1080p.mkv")
	}))
	t.Cleanup(r.cdn.Close)
	r.addon = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.addonHeaders = append(r.addonHeaders, req.Header.Get("X-Addon-Key"))
		r.mu.Unlock()
		switch req.URL.Path {
		case "/" + addonKey + "/resolve":
			r.resolves.Add(1)
			http.Redirect(w, req, "http://cdn.test/dl/Movie.2014.1080p.mkv", http.StatusFound)
		case "/" + addonKey + "/same-origin.mkv":
			serve(w, req, directSize, "same-origin.mkv")
		case "/" + addonKey + "/placeholder":
			serve(w, req, 2<<20, "downloading.mp4")
		case "/" + addonKey + "/html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>not cached</html>")
		case "/" + addonKey + "/norange":
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", "100")
			fmt.Fprint(w, strings.Repeat("x", 100))
		case "/" + addonKey + "/nosize":
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, "x")
		case "/" + addonKey + "/private":
			http.Redirect(w, req, "http://internal.test/x.mkv", http.StatusFound)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.addon.Close)
	guard := &safefetch.Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "cdn.test":
				return []netip.Addr{netip.MustParseAddr(publicIP)}, nil
			case "internal.test":
				return []netip.Addr{netip.MustParseAddr("10.0.0.7")}, nil
			}
			return nil, fmt.Errorf("unknown host %q", host)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			var d net.Dialer
			if host == publicIP {
				return d.DialContext(ctx, network, r.cdn.Listener.Addr().String())
			}
			if host == "127.0.0.1" {
				return d.DialContext(ctx, network, addr)
			}
			return nil, fmt.Errorf("guard dialed %q", addr)
		},
	}
	r.svc = NewService(cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DirectGuard: guard})
	return r
}

func (r *directRig) release(path string) core.Release {
	return core.Release{ID: "http-" + path, Name: "Movie.2014.1080p.WEB-DL-GRP", Protocol: core.ProtocolHTTP,
		StreamURL: r.addon.URL + "/" + addonKey + "/" + path, StreamOrigin: r.addon.URL,
		StreamHeaders: map[string]string{"X-Addon-Key": addonKey}, Size: directSize}
}

func TestDirectPayloadProbesTheAddonLink(t *testing.T) {
	r := newDirectRig(t, config.Debrid{CachedOnly: true})
	raw, err := r.svc.DirectPayload(context.Background(), r.release("resolve"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := DecodePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsDirect() || p.Size != directSize || p.Name() != "Movie.2014.1080p.mkv" || p.URL != "http://cdn.test/dl/Movie.2014.1080p.mkv" || p.Link != r.addon.URL+"/"+addonKey+"/resolve" {
		t.Fatalf("payload %+v", p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.addonHeaders) != 1 || r.addonHeaders[0] != addonKey {
		t.Fatalf("addon request headers %v", r.addonHeaders)
	}
	if len(r.cdnHeaders) != 1 || r.cdnHeaders[0] != "" {
		t.Fatalf("addon header followed the redirect to another origin: %v", r.cdnHeaders)
	}
}

func TestDirectPayloadRejectsWhatIsNotTheRelease(t *testing.T) {
	r := newDirectRig(t, config.Debrid{CachedOnly: true})
	for path, want := range map[string]error{
		"placeholder": core.ErrNotCached,
		"html":        core.ErrNotCached,
		"norange":     core.ErrSourceUnavailable,
		"nosize":      core.ErrSourceUnavailable,
		"missing":     core.ErrSourceUnavailable,
		"private":     core.ErrSourceUnavailable,
	} {
		_, err := r.svc.DirectPayload(context.Background(), r.release(path))
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", path, err, want)
		}
		if err != nil && strings.Contains(err.Error(), addonKey) {
			t.Errorf("%s: error leaks the link: %v", path, err)
		}
		if errors.Is(err, core.ErrArticleMissing) || errors.Is(err, core.ErrArticleCorrupt) || errors.Is(err, core.ErrReleaseRejected) {
			t.Errorf("%s: transient answer would mark the release bad: %v", path, err)
		}
	}
	// Only a malformed link is the release's own fault.
	bad := r.release("x")
	bad.StreamURL = "stremio:///detail/movie/tt1"
	if _, err := r.svc.DirectPayload(context.Background(), bad); !errors.Is(err, core.ErrReleaseRejected) {
		t.Fatalf("malformed link: %v", err)
	}
	// A file far smaller than the size the addon announced is a placeholder.
	rel := r.release("same-origin.mkv")
	rel.Size = 3 * directSize
	if _, err := r.svc.DirectPayload(context.Background(), rel); !errors.Is(err, core.ErrNotCached) {
		t.Fatalf("undersized file: %v", err)
	}
	// Only the addon's own origin is trusted: a private link elsewhere is refused.
	rel = r.release("same-origin.mkv")
	rel.StreamOrigin = "http://other.example"
	if _, err := r.svc.DirectPayload(context.Background(), rel); err == nil {
		t.Fatal("loopback link fetched without its origin being trusted")
	}
}

func TestEngineReadsDirectStreamsAndRefreshesThroughTheLink(t *testing.T) {
	// Without read-ahead each read below is its own request.
	r := newDirectRig(t, config.Debrid{CachedOnly: true, ReadAheadChunks: -1})
	eng := NewEngine(r.svc, EngineOptions{Inner: &fakeInner{}, BlockSize: testBlock, HedgeDelay: -1, Logger: r.svc.log})
	defer eng.Close()
	rel := r.release("resolve")
	raw, err := r.svc.DirectPayload(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	src, err := eng.Open(context.Background(), rel.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	rc := src.(interface {
		ReadAtContext(context.Context, []byte, int64) (int, error)
	})
	check := func(off int64) {
		t.Helper()
		buf := make([]byte, 4096)
		n, err := rc.ReadAtContext(context.Background(), buf, off)
		if err != nil && err != io.EOF {
			t.Fatalf("read at %d: %v", off, err)
		}
		for i := 0; i < n; i++ {
			if buf[i] != byte((off+int64(i))%251) {
				t.Fatalf("byte %d wrong", off+int64(i))
			}
		}
	}
	if src.Size() != directSize || src.Name() != "Movie.2014.1080p.mkv" {
		t.Fatalf("source %d %q", src.Size(), src.Name())
	}
	check(0)
	check(17 << 20)
	if r.resolves.Load() != 1 {
		t.Fatalf("media reads went back through the addon's resolver: %d resolves", r.resolves.Load())
	}
	// The CDN link expires: the engine asks the addon's link again.
	r.cdnFail.Store(1)
	check(30 << 20)
	if r.resolves.Load() != 2 {
		t.Fatalf("expired redirect target not re-resolved: %d resolves", r.resolves.Load())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.cdnHeaders {
		if h != "" {
			t.Fatal("addon header sent to the CDN")
		}
	}
}

func TestIndexerPlaysAddonLinksWithoutDebridAccounts(t *testing.T) {
	r := newDirectRig(t, config.Debrid{CachedOnly: true})
	link := r.release("resolve")
	torrentWithLink := link
	torrentWithLink.ID, torrentWithLink.Protocol, torrentWithLink.InfoHash = "t-link", core.ProtocolTorrent, testHash
	bareTorrent := core.Release{ID: "t-bare", Name: "Movie.2014.1080p", Protocol: core.ProtocolTorrent, InfoHash: testHash}
	uncached := link
	uncached.ID, uncached.DebridUncached = "http-uncached", []string{config.DebridRealDebrid}
	usenet := core.Release{ID: "nzb", Name: "Movie.2014.1080p", NZBURL: "https://ix.example/x.nzb"}
	inner := &fakeInner{results: []core.Release{link, torrentWithLink, bareTorrent, uncached, usenet}}
	x := NewIndexer(inner, r.svc)
	usenetReady := false
	x.SetUsenetReady(func() bool { return usenetReady })
	ids := func() map[string]string {
		rels, err := x.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, Title: "Movie"})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, rel := range rels {
			out[rel.ID] = rel.Protocol
		}
		return out
	}
	got := ids()
	want := map[string]string{"http-resolve": core.ProtocolHTTP, "t-link": core.ProtocolHTTP}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("cached-only, no providers: %v, want %v", got, want)
	}
	// With cached-only off the owner accepts links the addon labels uncached.
	r.svc.UpdateConfig(config.Debrid{CachedOnly: false})
	usenetReady = true
	got = ids()
	if got["http-uncached"] != core.ProtocolHTTP || got["nzb"] != core.ProtocolUsenet || len(got) != 4 {
		t.Fatalf("cached-only off, Usenet ready: %v", got)
	}
	// An HTTP release plays through its link, not a provider.
	raw, err := x.FetchNZB(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := DecodePayload(raw); err != nil || !p.IsDirect() {
		t.Fatalf("payload %+v %v", p, err)
	}
	if inner.fetched.Load() != 0 {
		t.Fatal("HTTP release fetched through the Usenet indexer")
	}
}

func TestFailingAddonLinkIsNeverMarkedGone(t *testing.T) {
	r := newDirectRig(t, config.Debrid{CachedOnly: true, ReadAheadChunks: -1})
	eng := NewEngine(r.svc, EngineOptions{Inner: &fakeInner{}, BlockSize: testBlock, HedgeDelay: -1, Logger: r.svc.log})
	defer eng.Close()
	rel := r.release("resolve")
	raw, err := r.svc.DirectPayload(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	src, err := eng.Open(context.Background(), rel.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	r.cdnFail.Store(1 << 20) // the CDN refuses every request from now on
	_, err = src.(interface {
		ReadAtContext(context.Context, []byte, int64) (int, error)
	}).ReadAtContext(context.Background(), make([]byte, 10), 20<<20)
	if err == nil || errors.Is(err, core.ErrArticleMissing) || errors.Is(err, core.ErrArticleCorrupt) || !errors.Is(err, core.ErrSourceUnavailable) {
		t.Fatalf("failing addon link: %v", err)
	}
}
