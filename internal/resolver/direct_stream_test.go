package resolver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
)

// zeroFile is a seekable file of size zero bytes.
type zeroFile struct{ size, off int64 }

func (z *zeroFile) Read(b []byte) (int, error) {
	if z.off >= z.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(b)), z.size-z.off))
	clear(b[:n])
	z.off += int64(n)
	return n, nil
}

func (z *zeroFile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		z.off = off
	case io.SeekCurrent:
		z.off += off
	case io.SeekEnd:
		z.off = z.size + off
	}
	return z.off, nil
}

// An addon that first answers without range support (a proxy hiccup, a
// file it is still preparing) and then serves ranged media: the first
// failure is transient, so the release is neither marked bad nor skipped as
// recently failed, and it plays on the next attempt.
func TestTransientAddonAnswerNeverMarksTheReleaseBad(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		if hits.Add(1) == 1 {
			w.Header().Set("Content-Length", "1024")
			_, _ = w.Write(make([]byte, 1024)) // 200, ignoring Range
			return
		}
		http.ServeContent(w, r, "The.Signal.1999.1080p.WEB-DL.H264-GRP.mkv", time.Time{}, &zeroFile{size: 64 << 20})
	}))
	defer srv.Close()

	cfg := testCfg()
	cfg.Debrid = config.Debrid{CachedOnly: true} // no debrid account: the addon link is the source
	svc := debrid.NewService(cfg.Debrid, debrid.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	eng := debrid.NewEngine(svc, debrid.EngineOptions{Inner: noopEngine{}, HedgeDelay: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer eng.Close()
	stream := core.Release{ID: "addon-http", GUID: "http:ix:1", Indexer: "addon", Name: "The.Signal.1999.1080p.WEB-DL.H264-GRP",
		Protocol: core.ProtocolHTTP, StreamURL: srv.URL + "/play/SECRET", StreamOrigin: srv.URL, PubDate: time.Now()}
	stream.Info = withParsed(stream).Info
	idx := debrid.NewIndexer(&fakeIndexer{releases: []core.Release{stream}}, svc)
	s := New(idx, eng, nil, cfg, Options{})
	defer s.Close()

	title := matrixTitle()
	if _, err := s.Resolve(context.Background(), core.ResolveRequest{Title: title, Client: webCaps()}); err == nil {
		t.Fatal("a 200 without range support resolved")
	}
	if s.isBad(stream.ID) || s.recentlyFailedHealth(stream.ID) {
		t.Fatalf("transient addon answer persisted: bad=%v recentlyFailed=%v", s.isBad(stream.ID), s.recentlyFailedHealth(stream.ID))
	}
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: title, Client: webCaps()})
	if err != nil {
		t.Fatalf("retry after the addon recovered: %v", err)
	}
	if res.Release.ID != stream.ID || res.Release.Protocol != core.ProtocolHTTP {
		t.Fatalf("resolved %+v", res.Release)
	}
}

// noopEngine stands in for the Usenet engine; it is never reached.
type noopEngine struct{}

func (noopEngine) Open(context.Context, string, []byte) (core.MediaSource, error) {
	return nil, core.ErrSourceUnavailable
}
func (noopEngine) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }
