package sessions

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/webdav"
	"github.com/braedonsaunders/ether/internal/store"
)

type webdavGrant struct {
	mu      sync.Mutex
	revoked bool
}

func (g *webdavGrant) GrantReference(_ context.Context, surface, credential string) (string, *core.Viewer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked || surface != "webdav" || credential != "grant" {
		return "", nil, core.ErrUnauthorized
	}
	return "reference", &core.Viewer{ID: 1}, nil
}
func (g *webdavGrant) ValidateGrantReference(_ context.Context, surface, reference string, viewer int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked || surface != "webdav" || reference != "reference" || viewer != 1 {
		return core.ErrUnauthorized
	}
	return nil
}

type rangedPlayback struct{ *fakePlayback }

func (p *rangedPlayback) ServeRange(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Header.Get("Range") != "bytes=0-2" {
		http.Error(w, "missing range", 400)
		return
	}
	w.Header().Set("Content-Range", "bytes 0-2/3")
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.WriteString(w, "abc")
}

func TestWebDAVAuthenticatedRangesUseBrokerAndRevoke(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &rangedPlayback{&fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}}
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	broker, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	grants := &webdavGrant{}
	lazy, err := NewLazyStreams(db, broker, "webdav", grants, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	movie := frontend.Title{Key: core.MovieKey(42), Type: core.TypeMovie, Title: "Film", Year: 2024}
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			_, viewer, err := grants.GrantReference(ctx, "webdav", token)
			return viewer, err
		}},
		Experience: frontendtest.Experience{
			ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
				return []frontend.View{{ID: "movies", Type: core.TypeMovie}}, nil
			},
			LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
				return frontend.Page{Items: []frontend.Title{movie}, Total: 1}, nil
			},
		},
		Sessions: broker, LazyStreams: lazy, Capabilities: frontendtest.Capabilities{}, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.9"), true },
	}
	inst, err := (webdav.Frontend{}).Mount(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(inst.Handler())
	defer server.Close()
	call := func(method string) (int, string) {
		r, err := http.NewRequest(method, server.URL+"/webdav/Movies/Film%20%282024%29/Film%20%282024%29.mkv", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.SetBasicAuth("1", "grant")
		r.Header.Set("Range", "bytes=0-2")
		if method == "PROPFIND" {
			r.Header.Set("Depth", "0")
		}
		res, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		return res.StatusCode, string(body)
	}
	if code, _ := call("PROPFIND"); code != 207 {
		t.Fatalf("PROPFIND %d", code)
	}
	if code, _ := call(http.MethodHead); code != 200 || pb.prepares != 0 {
		t.Fatalf("HEAD %d opened %d", code, pb.prepares)
	}
	for n := 0; n < 2; n++ {
		if code, body := call(http.MethodGet); code != 206 || body != "abc" {
			t.Fatalf("range %d %q", code, body)
		}
	}
	if pb.prepares != 1 || len(pb.Sessions()) != 1 {
		t.Fatalf("range reuse: prepares=%d sessions=%d", pb.prepares, len(pb.Sessions()))
	}
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	if code, _ := call(http.MethodGet); code != 401 {
		t.Fatalf("revoked grant %d", code)
	}
}
