package resolver

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
)

// streamingIndexer emits its results through SearchWith so the resolver's
// speculative (background-budget) checks run, as the real indexer fleet does.
type streamingIndexer struct{ fakeIndexer }

func (l *streamingIndexer) SearchWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	rs, err := l.Search(ctx, req)
	if arrived != nil && len(rs) > 0 {
		arrived(rs)
	}
	return rs, err
}

// realDebridLike behaves like Real-Debrid for this case: no bulk availability
// check, an account listing that holds only the low release, and instant
// availability for the high-scoring WEB-DL once it is actually added.
type realDebridLike struct {
	mu       sync.Mutex
	inAcct   map[string]string // hash -> torrent id
	acquires []string
}

func (p *realDebridLike) Kind() string { return config.DebridRealDebrid }
func (p *realDebridLike) CheckCached(context.Context, []string) (map[string]bool, error) {
	return nil, debrid.ErrUnsupported
}
func (p *realDebridLike) Find(_ context.Context, hash string) (string, error) {
	return p.inAcct[hash], nil
}
func (p *realDebridLike) Acquire(ctx context.Context, req debrid.AcquireRequest) (*debrid.Acquired, error) {
	p.mu.Lock()
	p.acquires = append(p.acquires, req.InfoHash)
	p.mu.Unlock()
	id := req.TorrentID
	added := false
	if id == "" {
		id, added = "added-"+req.InfoHash[:4], true
	}
	f, err := req.Choose([]debrid.File{{ID: "1", Path: "/The.Signal.1999.mkv", Size: 4 << 30}})
	if err != nil {
		return nil, err
	}
	return &debrid.Acquired{TorrentID: id, File: f, Added: added}, nil
}
func (p *realDebridLike) Link(context.Context, debrid.Handle) (debrid.DirectLink, error) {
	return debrid.DirectLink{URL: "https://cdn.example/file", Size: 4 << 30, Expires: time.Now().Add(time.Hour)}, nil
}
func (p *realDebridLike) Remove(context.Context, string) error { return nil }
func (p *realDebridLike) Account(context.Context) (debrid.Account, error) {
	return debrid.Account{User: "lab", Premium: true}, nil
}

const hashTop = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Planned item A (debrid releases the background check cannot judge), on
// Real-Debrid: a background pre-check on Real-Debrid answers ErrUnsupported
// for a torrent not yet in the account. Playback must still try the
// higher-ranked release in the foreground instead of settling on the
// lower-ranked one that happens to be in the account already.
func TestRealDebridForegroundAttemptWhenBackgroundCheckUndecided(t *testing.T) {
	top := rel("top", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-TOP", 6000)
	top.Protocol, top.InfoHash, top.NZBURL = core.ProtocolTorrent, hashTop, ""
	low := rel("low", "The.Signal.1999.720p.HDTV.x264-LOW", 1500)
	low.Protocol, low.InfoHash, low.NZBURL = core.ProtocolTorrent, hashA, ""

	rd := &realDebridLike{inAcct: map[string]string{hashA: "acct-low"}}
	cfg := testCfg()
	cfg.Debrid = config.Debrid{CachedOnly: true, Providers: []config.DebridProvider{{ID: "rd", Kind: config.DebridRealDebrid, APIKey: "k", Enabled: true}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := debrid.NewService(cfg.Debrid, debrid.Options{Logger: logger,
		NewProvider: func(config.DebridProvider, debrid.ProviderOptions) (debrid.Provider, error) { return rd, nil }})
	idx := debrid.NewIndexer(&streamingIndexer{fakeIndexer{releases: []core.Release{top, low}}}, svc)
	s := New(idx, fakeHealth{}, nil, cfg, Options{})
	defer s.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	rd.mu.Lock()
	tried := append([]string(nil), rd.acquires...)
	rd.mu.Unlock()
	if err != nil || got.Release.ID != "top" {
		var id string
		if got != nil {
			id = got.Release.ID
		}
		t.Fatalf("resolved %q (err %v); provider acquires %v: the top release was never tried in the foreground", id, err, tried)
	}
}
