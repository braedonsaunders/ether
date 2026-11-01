package debrid

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// mediaServer serves one file at /m/{token} with byte ranges. Tokens are
// links a fake provider handed out; revoked tokens answer 403.
type mediaServer struct {
	*httptest.Server
	data    []byte
	latency time.Duration // per request, before headers
	pace    time.Duration // gap between 64 KiB body chunks, after the first
	// sleep replaces time.Sleep for latency. Tests that assert a startup
	// shape record the delay instead of waiting on the wall clock.
	sleep func(time.Duration)

	mu     sync.Mutex
	valid  map[string]bool
	status map[string]int // forced status per token
	stall  map[string]time.Duration
	// stallAfter, keyed token+"|"+Range like stall, stalls one ranged
	// response mid-body: headers and that many bytes go out, then the
	// handler blocks until the client disconnects.
	stallAfter map[string]int64
	size       int64 // advertised total (0 = real)
	requests   atomic.Int64
	ranges     []string
	// stallEvery, when > 0, stalls every ranged response mid-body after
	// that many bytes, until the client disconnects.
	stallEvery atomic.Int64
	// inflight counts handlers currently running, for leak assertions.
	inflight atomic.Int32
}

func newMediaServer(t testing.TB, size int) *mediaServer {
	data := make([]byte, size)
	rand.New(rand.NewSource(1)).Read(data)
	m := &mediaServer{data: data, valid: map[string]bool{}, status: map[string]int{}, stall: map[string]time.Duration{}, stallAfter: map[string]int64{}}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.Close)
	return m
}

func (m *mediaServer) serve(w http.ResponseWriter, r *http.Request) {
	m.inflight.Add(1)
	defer m.inflight.Add(-1)
	m.requests.Add(1)
	token := strings.TrimPrefix(r.URL.Path, "/m/")
	m.mu.Lock()
	m.ranges = append(m.ranges, r.Header.Get("Range"))
	// Stall only the first request for a token (optionally a range prefix).
	key := token
	if _, ok := m.stall[token+"|"+r.Header.Get("Range")]; ok {
		key = token + "|" + r.Header.Get("Range")
	}
	ok, status, stall, size := m.valid[token], m.status[token], m.stall[key], m.size
	delete(m.stall, key)
	stallAfter, stallOnce := m.stallAfter[token+"|"+r.Header.Get("Range")]
	if stallOnce {
		delete(m.stallAfter, token+"|"+r.Header.Get("Range"))
	}
	m.mu.Unlock()
	if n := m.stallEvery.Load(); n > 0 && !stallOnce {
		stallAfter, stallOnce = n, true
	}
	m.wait(m.latency)
	if stall > 0 {
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if stallOnce {
		// A real upstream stall over the socket: valid 206 headers and
		// the first stallAfter bytes, flushed, then silence — the
		// connection stays open until the client gives up on it.
		var a, b int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b)
		total := int64(len(m.data))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", b-a+1))
		w.WriteHeader(http.StatusPartialContent)
		end := min(a+stallAfter, b+1, total)
		if end > a {
			w.Write(m.data[a:end])
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
		return
	}
	if size > 0 {
		var a, b int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, size))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(m.data[a : b+1])
		return
	}
	var body io.ReadSeeker = bytes.NewReader(m.data)
	if m.pace > 0 {
		body = &pacedReader{r: bytes.NewReader(m.data), pace: m.pace}
	}
	http.ServeContent(w, r, "f.mkv", time.Time{}, body)
}

// pacedReader delivers the first 64 KiB immediately and then waits between
// chunks, so a test can tell a first-block return from a full-run wait.
type pacedReader struct {
	r    *bytes.Reader
	pace time.Duration
	n    int
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if p.n > 0 && p.pace > 0 {
		time.Sleep(p.pace)
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	n, err := p.r.Read(b)
	p.n += n
	return n, err
}

func (p *pacedReader) Seek(offset int64, whence int) (int64, error) {
	return p.r.Seek(offset, whence)
}

func (m *mediaServer) wait(d time.Duration) {
	if d <= 0 {
		return
	}
	if m.sleep != nil {
		m.sleep(d)
		return
	}
	time.Sleep(d)
}

func (m *mediaServer) issue(token string) string {
	m.mu.Lock()
	m.valid[token] = true
	m.mu.Unlock()
	return m.URL + "/m/" + token
}

func (m *mediaServer) revokeAll() {
	m.mu.Lock()
	m.valid = map[string]bool{}
	m.mu.Unlock()
}

// fakeProvider is an in-memory debrid service handing out mediaServer links.
type fakeProvider struct {
	kind  string
	delay time.Duration // API latency for Link
	// sleep replaces time.Sleep for delay and acqDelay.
	sleep func(time.Duration)
	media *mediaServer
	files []File
	ttl   time.Duration
	now   func() time.Time

	mu        sync.Mutex
	cached    map[string]bool
	bulk      bool
	authFail  bool
	gone      bool // Link answers ErrGone until re-acquired
	torrents  map[string]string
	calls     map[string]int
	tokens    int
	removed   int
	lastAdded string
	adds      int           // torrents added to the account
	acqDelay  time.Duration // Acquire latency before touching the account
	// downloading counts uncached torrents left on the account (cached-only off).
	downloading int
}

func newFakeProvider(kind string, media *mediaServer, files []File) *fakeProvider {
	return &fakeProvider{kind: kind, media: media, files: files, ttl: time.Hour, now: time.Now,
		cached: map[string]bool{}, torrents: map[string]string{}, calls: map[string]int{}}
}

func (p *fakeProvider) wait(d time.Duration) {
	if d <= 0 {
		return
	}
	if p.sleep != nil {
		p.sleep(d)
		return
	}
	time.Sleep(d)
}

func (p *fakeProvider) count(op string) {
	p.mu.Lock()
	p.calls[op]++
	p.mu.Unlock()
}

func (p *fakeProvider) callCount(op string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[op]
}

func (p *fakeProvider) totalCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		n += c
	}
	return n
}

func (p *fakeProvider) Kind() string { return p.kind }

func (p *fakeProvider) CheckCached(_ context.Context, hashes []string) (map[string]bool, error) {
	p.count("check")
	if !p.bulk {
		return nil, ErrUnsupported
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]bool{}
	for _, h := range hashes {
		out[h] = p.cached[h]
	}
	return out, nil
}

func (p *fakeProvider) Find(_ context.Context, hash string) (string, error) {
	p.count("find")
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.torrents[hash], nil
}

func (p *fakeProvider) Acquire(_ context.Context, req AcquireRequest) (*Acquired, error) {
	p.count("acquire")
	p.wait(p.acqDelay)
	p.mu.Lock()
	if p.authFail {
		p.mu.Unlock()
		return nil, fmt.Errorf("fake: %w", ErrAuth)
	}
	if !p.cached[req.InfoHash] {
		if !req.CachedOnly && p.torrents[req.InfoHash] == "" {
			// Left on the account to download, like the real services.
			p.torrents[req.InfoHash] = fmt.Sprintf("T%d", len(p.torrents)+p.removed+1)
			p.adds++
			p.downloading++
		}
		p.mu.Unlock()
		return nil, fmt.Errorf("fake: %w", core.ErrNotCached)
	}
	added := false
	id := req.TorrentID
	if id == "" || p.torrents[req.InfoHash] != id {
		id = fmt.Sprintf("T%d", len(p.torrents)+p.removed+1)
		p.torrents[req.InfoHash] = id
		added = true
		p.adds++
		p.lastAdded = id
	}
	p.gone = false
	files := append([]File(nil), p.files...)
	p.mu.Unlock()
	f, err := req.Choose(files)
	if err != nil {
		return nil, err
	}
	return &Acquired{TorrentID: id, File: f, Added: added}, nil
}

func (p *fakeProvider) Link(_ context.Context, h Handle) (DirectLink, error) {
	p.count("link")
	p.wait(p.delay)
	p.mu.Lock()
	if p.authFail {
		p.mu.Unlock()
		return DirectLink{}, fmt.Errorf("fake: %w", ErrAuth)
	}
	if p.gone || p.torrents[h.InfoHash] != h.TorrentID {
		p.mu.Unlock()
		return DirectLink{}, fmt.Errorf("fake: %w", ErrGone)
	}
	p.tokens++
	token := fmt.Sprintf("%s-%d", p.kind, p.tokens)
	p.mu.Unlock()
	return DirectLink{URL: p.media.issue(token), Size: h.Size, Expires: p.now().Add(p.ttl)}, nil
}

func (p *fakeProvider) Remove(_ context.Context, id string) error {
	p.count("remove")
	p.mu.Lock()
	defer p.mu.Unlock()
	for h, v := range p.torrents {
		if v == id {
			delete(p.torrents, h)
			p.removed++
		}
	}
	return nil
}

func (p *fakeProvider) Account(context.Context) (Account, error) {
	p.count("account")
	if p.authFail {
		return Account{}, ErrAuth
	}
	return Account{User: "owner", Premium: true}, nil
}

// memCache is an in-memory core.SegmentCache with Has.
type memCache struct {
	mu   sync.Mutex
	segs map[string][]byte
	puts atomic.Int64
}

func newMemCache() *memCache { return &memCache{segs: map[string][]byte{}} }

func (c *memCache) key(id string, seg int) string { return fmt.Sprintf("%s/%d", id, seg) }

func (c *memCache) Get(id string, seg int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.segs[c.key(id, seg)]
	return append([]byte(nil), b...), ok
}

func (c *memCache) Has(id string, seg int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.segs[c.key(id, seg)]
	return ok
}

func (c *memCache) Put(id string, seg int, data []byte) error {
	c.puts.Add(1)
	c.mu.Lock()
	c.segs[c.key(id, seg)] = append([]byte(nil), data...)
	c.mu.Unlock()
	return nil
}
func (c *memCache) Pin(string, string) error   { return nil }
func (c *memCache) Unpin(string, string) error { return nil }
func (c *memCache) Usage() (int64, int64)      { return 0, 0 }

// fakeInner is the Usenet engine/indexer stand-in.
type fakeInner struct {
	opened, prefetched, checked, searched, fetched atomic.Int64
	results                                        []core.Release
}

func (f *fakeInner) Open(context.Context, string, []byte) (core.MediaSource, error) {
	f.opened.Add(1)
	return nil, fmt.Errorf("fake inner open")
}
func (f *fakeInner) Prefetch(context.Context, string, []byte, int64, int64) error {
	f.prefetched.Add(1)
	return nil
}
func (f *fakeInner) Check(context.Context, []byte) (bool, error) { f.checked.Add(1); return true, nil }
func (f *fakeInner) Name() string                                { return "indexers" }
func (f *fakeInner) Search(context.Context, core.SearchRequest) ([]core.Release, error) {
	f.searched.Add(1)
	return append([]core.Release(nil), f.results...), nil
}
func (f *fakeInner) FetchNZB(context.Context, core.Release) ([]byte, error) {
	f.fetched.Add(1)
	return []byte("<nzb/>"), nil
}

// harness wires a Service with fake providers over a shared media server.
type harness struct {
	t     testing.TB
	media *mediaServer
	provs map[string]*fakeProvider
	svc   *Service
	db    interface{ Close() error }
}

func movieFiles(size int64) []File {
	return []File{{ID: "1", Path: "Movie.2024/Movie.2024.1080p.mkv", Size: size}, {ID: "2", Path: "Movie.2024/sample.mkv", Size: 1000}}
}

func newHarness(t testing.TB, size int, kinds ...string) *harness {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, media: newMediaServer(t, size), provs: map[string]*fakeProvider{}, db: db}
	t.Cleanup(func() { db.Close() })
	var cfg config.Debrid
	cfg.CachedOnly, cfg.RemoveUncached = true, true
	for i, k := range kinds {
		id := fmt.Sprintf("%s-%d", k, i)
		h.provs[id] = newFakeProvider(k, h.media, movieFiles(int64(size)))
		cfg.Providers = append(cfg.Providers, config.DebridProvider{ID: id, Name: id, Kind: k, APIKey: "k", Priority: i, Enabled: true})
	}
	h.svc = NewService(cfg, Options{DB: db, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), NewProvider: func(c config.DebridProvider, _ ProviderOptions) (Provider, error) {
		if p := h.provs[c.ID]; p != nil {
			return p, nil
		}
		return nil, fmt.Errorf("unknown %s", c.ID)
	}})
	return h
}

func (h *harness) cacheEverywhere(hash string) {
	for _, p := range h.provs {
		p.mu.Lock()
		p.cached[hash] = true
		p.mu.Unlock()
	}
}

func torrentRelease(id string) core.Release {
	return core.Release{ID: id, Name: "Movie.2024.1080p.WEB-DL-GRP", Indexer: "tz", Protocol: core.ProtocolTorrent, InfoHash: testHash, Size: 4 << 30}
}
