package frontendadapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plugins"
)

func TestDownloadPreparationHold(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wait       time.Duration
		disconnect bool
		byteRange  string
		wantBody   string
		status     int
	}{
		{name: "ready after pending flights", wait: 4 * time.Second, wantBody: "ready", status: http.StatusOK},
		{name: "range after pending flights", wait: 4 * time.Second, byteRange: "bytes=0-2", wantBody: "rea", status: http.StatusPartialContent},
		{name: "wait expires", wait: 2 * time.Second, status: http.StatusServiceUnavailable},
		{name: "disconnect releases hold", wait: 4 * time.Second, disconnect: true},
		{name: "player returns pending", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				issuer := NewTicketIssuer()
				startJob := make(chan struct{})
				jobDone := make(chan struct{})
				go func() {
					<-startJob
					time.Sleep(3 * time.Second)
					close(jobDone)
				}()
				var startOnce sync.Once
				h := NewMediaHandler(MediaDeps{
					Tickets: issuer,
					Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
						return &core.Viewer{ID: id}, nil
					},
					Open: func(ctx context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
						startOnce.Do(func() { close(startJob) })
						if plugins.FrontendPreparationHold(ctx) != (tc.wait > 0) {
							t.Error("preparation hold context does not match grant")
						}
						// Model a shared resolve flight ending before the
						// job is done. Only the open waits on this context.
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-time.After(500 * time.Millisecond):
						}
						select {
						case <-jobDone:
							return &frontend.KindMedia{ReaderAt: strings.NewReader("ready"), Closer: io.NopCloser(strings.NewReader("")), Size: 5}, nil
						default:
							return nil, core.PreparingFromPending(250)
						}
					},
				})
				token, err := issuer.Issue(StreamGrant{ViewerID: 1, ItemID: "item", Adapter: "plugin", Wait: tc.wait})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req := httptest.NewRequestWithContext(ctx, http.MethodGet, KindMediaPath+token, nil)
				req.Header.Set("Range", tc.byteRange)
				rec := &httptest.ResponseRecorder{Body: new(bytes.Buffer)}
				watched := &headerWatch{ResponseRecorder: rec}
				served := make(chan struct{})
				go func() {
					defer close(served)
					h.ServeHTTP(watched, req)
				}()
				if tc.wait > 0 {
					synctest.Wait()
					// The handler keeps running after Wait; only the atomic flag is
					// safe to read before it returns.
					if watched.wrote.Load() {
						t.Error("sent response headers while the job was pending")
					}
				}
				if tc.disconnect {
					cancel()
				}
				<-served
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d", rec.Code, tc.status)
				}
				if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
					t.Fatalf("body = %q, want %q", rec.Body.String(), tc.wantBody)
				}
				if tc.status == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
					t.Fatal("pending response has no retry hint")
				}
				if tc.disconnect {
					select {
					case <-jobDone:
						t.Fatal("wait returned after the job had already finished")
					default:
					}
				}
				if issuer.waits.total != 0 {
					t.Fatal("wait slot leaked")
				}
				<-jobDone
			})
		})
	}
}

func TestDownloadPreparationLimitsAreShared(t *testing.T) {
	issuer := NewTicketIssuer()
	h := NewMediaHandler(MediaDeps{
		Tickets: issuer,
		Viewer: func(_ context.Context, id int64) (*core.Viewer, error) {
			return &core.Viewer{ID: id}, nil
		},
		Open: func(ctx context.Context, _ *core.Viewer, _ string, _ []string) (*frontend.KindMedia, error) {
			if plugins.FrontendPreparationHold(ctx) {
				t.Error("request over the limit was held")
				return nil, core.ErrNotAvailable
			}
			return nil, core.ErrPrepareSlow
		},
	})
	check := func(plugin string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeGrant(rec, httptest.NewRequest(http.MethodGet, "/", nil), StreamGrant{
			ViewerID: 1, ItemID: "item", Adapter: plugin, Wait: time.Minute,
		})
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("over capacity: status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	for i := range maxHeldGlobally {
		plugin := fmt.Sprintf("plugin-%d", i/maxHeldPerPlugin)
		if !issuer.waits.acquire(plugin) {
			t.Fatalf("failed to reserve available slot %d", i)
		}
		if i == maxHeldPerPlugin-1 {
			check(plugin)
		}
	}
	check("another-plugin")
}

// headerWatch records, race-free, whether the handler started a response.
type headerWatch struct {
	*httptest.ResponseRecorder
	wrote atomic.Bool
}

func (w *headerWatch) WriteHeader(code int) {
	w.wrote.Store(true)
	w.ResponseRecorder.WriteHeader(code)
}

func (w *headerWatch) Write(b []byte) (int, error) {
	w.wrote.Store(true)
	return w.ResponseRecorder.Write(b)
}
