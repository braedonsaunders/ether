package sessions

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLANLazyStreamsBindPeerViewerAndPolicy(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now()
	selected := int64(1)
	hidden := false
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id != selected {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id}, nil
	}
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	broker, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: func() time.Time { return now }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		if hidden {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: key}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	clientIP := func(r *http.Request) (netip.Addr, bool) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return netip.Addr{}, false
		}
		ip, err := netip.ParseAddr(host)
		return ip, err == nil
	}
	lanViewer := func(_ context.Context, r *http.Request) (*core.Viewer, error) {
		ip, ok := clientIP(r)
		if !ok || !ip.IsPrivate() || selected == 0 {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: selected}, nil
	}
	issuer, err := NewLANLazyStreams(db, broker, clientIP, lanViewer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, peer string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = net.JoinHostPort(peer, "3333")
		return r
	}
	plan := frontend.PlanRequest{Protocol: "dlna", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
	issueRequest := request(http.MethodGet, "/dlna/browse", "192.168.1.3")
	foreign := plan
	foreign.Viewer = &core.Viewer{ID: 2}
	if _, err := issuer.IssueLAN(ctx, issueRequest, foreign); err == nil {
		t.Fatal("foreign viewer accepted")
	}
	foreign = plan
	foreign.Device.ID = "forged"
	if _, err := issuer.IssueLAN(ctx, issueRequest, foreign); err == nil {
		t.Fatal("forged device accepted")
	}
	spoof := request(http.MethodGet, "/dlna/browse", "203.0.113.1")
	spoof.Header.Set("X-Forwarded-For", "192.168.1.3")
	if _, err := issuer.IssueLAN(ctx, spoof, plan); err == nil {
		t.Fatal("spoofed LAN accepted")
	}
	path, err := issuer.IssueLAN(ctx, issueRequest, plan)
	if err != nil || !strings.HasPrefix(path, "/sessions/lazy/") || pb.prepares != 0 {
		t.Fatalf("issue %q: %v", path, err)
	}
	serve := func(method, peer string) int {
		w := httptest.NewRecorder()
		issuer.ServeHTTP(w, request(method, path, peer))
		return w.Code
	}
	if status := serve(http.MethodHead, "192.168.1.3"); status != 200 || pb.prepares != 0 {
		t.Fatalf("HEAD %d opened %d", status, pb.prepares)
	}
	if status := serve(http.MethodGet, "192.168.1.4"); status != 404 {
		t.Fatalf("foreign peer %d", status)
	}
	if status := serve(http.MethodGet, "192.168.1.3"); status != 200 || pb.prepares != 1 {
		t.Fatalf("GET %d opens %d", status, pb.prepares)
	}
	if status := serve(http.MethodGet, "192.168.1.3"); status != 200 || pb.prepares != 1 {
		t.Fatalf("reuse %d opens %d", status, pb.prepares)
	}
	hidden = true
	if status := serve(http.MethodGet, "192.168.1.3"); status != 404 {
		t.Fatalf("hidden title %d", status)
	}
	hidden = false
	selected = 2
	if status := serve(http.MethodGet, "192.168.1.3"); status != 404 {
		t.Fatalf("changed viewer %d", status)
	}
	selected = 1
	now = now.Add(time.Hour)
	if status := serve(http.MethodHead, "192.168.1.3"); status != 404 {
		t.Fatalf("expired %d", status)
	}
}
