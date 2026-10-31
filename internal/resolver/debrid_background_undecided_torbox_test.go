package resolver

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
)

// TorBox answers a bulk availability check (the top release is cached) but
// background checks still cannot read a torrent that is not in the account.
type torBoxLike struct{ realDebridLike }

func (p *torBoxLike) Kind() string { return config.DebridTorBox }
func (p *torBoxLike) CheckCached(_ context.Context, hs []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, h := range hs {
		out[h] = true
	}
	return out, nil
}

// Planned item A, on TorBox: the bulk check says cached, yet a background
// attempt cannot add the torrent, so playback must try it in the foreground.
func TestTorBoxForegroundAttemptWhenBackgroundCheckUndecided(t *testing.T) {
	top := rel("top", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-TOP", 6000)
	top.Protocol, top.InfoHash, top.NZBURL = core.ProtocolTorrent, hashTop, ""
	low := rel("low", "The.Signal.1999.720p.HDTV.x264-LOW", 1500)
	low.Protocol, low.InfoHash, low.NZBURL = core.ProtocolTorrent, hashA, ""
	tb := &torBoxLike{realDebridLike{inAcct: map[string]string{hashA: "acct-low"}}}
	cfg := testCfg()
	cfg.Debrid = config.Debrid{CachedOnly: true, Providers: []config.DebridProvider{{ID: "tb", Kind: config.DebridTorBox, APIKey: "k", Enabled: true}}}
	svc := debrid.NewService(cfg.Debrid, debrid.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewProvider: func(config.DebridProvider, debrid.ProviderOptions) (debrid.Provider, error) { return tb, nil }})
	s := New(debrid.NewIndexer(&streamingIndexer{fakeIndexer{releases: []core.Release{top, low}}}, svc), fakeHealth{}, nil, cfg, Options{})
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	tb.mu.Lock()
	tried := append([]string(nil), tb.acquires...)
	tb.mu.Unlock()
	if err != nil || got.Release.ID != "top" {
		var id string
		if got != nil {
			id = got.Release.ID
		}
		t.Fatalf("resolved %q (err %v); provider acquires %v: the top release was never tried in the foreground", id, err, tried)
	}
}
