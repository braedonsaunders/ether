package sessions

import (
	"context"
	"fmt"
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

type blockingLazyPlayback struct {
	*fakePlayback
	started chan struct{}
	finish  chan struct{}
}

func (p *blockingLazyPlayback) ServeRange(w http.ResponseWriter, r *http.Request, id string) {
	if p.started != nil {
		close(p.started)
		<-p.finish
	}
	p.fakePlayback.ServeRange(w, r, id)
}

func TestLANLazyStreamsBindPeerViewerAndPolicy(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now()
	selected := int64(1)
	hidden := false
	checks, revokeAfter := 0, 0
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id != selected {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id}, nil
	}
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &blockingLazyPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}}
	lookupCalls := 0
	broker, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: func() time.Time { return now }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		lookupCalls++
		if hidden {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: key}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	broker.deps.MediaLookup = func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		if hidden {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: key}, nil
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
		checks++
		if revokeAfter > 0 && checks >= revokeAfter {
			selected = 2
		}
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
	for n := 0; n < maxLazyCleanupReservations; n++ {
		if !broker.reserveLazyCleanup() {
			t.Fatal("reservation unavailable before capacity")
		}
	}
	full := httptest.NewRecorder()
	issuer.ServeHTTP(full, request(http.MethodGet, path, "192.168.1.3"))
	if full.Code != http.StatusServiceUnavailable || pb.prepares != 0 {
		t.Fatalf("saturated rollback capacity opened source: status=%d opens=%d", full.Code, pb.prepares)
	}
	for n := 0; n < maxLazyCleanupReservations; n++ {
		broker.releaseLazyCleanup()
	}
	checks, revokeAfter = 0, 3
	w := httptest.NewRecorder()
	issuer.ServeHTTP(w, request(http.MethodGet, path, "192.168.1.3"))
	if w.Code != http.StatusNotFound || len(pb.Sessions()) != 0 {
		t.Fatalf("selection changed during open: status=%d sessions=%d", w.Code, len(pb.Sessions()))
	}
	selected, revokeAfter = 1, 0
	pb.prepares = 0
	beforeHEAD := lookupCalls
	serve := func(method, peer string) int {
		w := httptest.NewRecorder()
		issuer.ServeHTTP(w, request(method, path, peer))
		return w.Code
	}
	if status := serve(http.MethodHead, "192.168.1.3"); status != 200 || pb.prepares != 0 {
		t.Fatalf("HEAD %d opened %d", status, pb.prepares)
	}
	if lookupCalls != beforeHEAD {
		t.Fatal("HEAD fetched network-capable metadata")
	}
	if status := serve(http.MethodGet, "192.168.1.4"); status != 404 {
		t.Fatalf("foreign peer %d", status)
	}
	if status := serve(http.MethodGet, "192.168.1.3"); status != 200 || pb.prepares != 1 {
		t.Fatalf("GET %d opens %d", status, pb.prepares)
	}
	beforeReuse := lookupCalls
	if status := serve(http.MethodGet, "192.168.1.3"); status != 200 || pb.prepares != 1 {
		t.Fatalf("reuse %d opens %d", status, pb.prepares)
	}
	if lookupCalls != beforeReuse {
		t.Fatal("reused GET fetched network-capable metadata")
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
	newPath, err := issuer.IssueLAN(ctx, issueRequest, plan)
	if err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("expired capability cleanup: count=%d err=%v", remaining, err)
	}
	broker.mu.Lock()
	var held *brokerSession
	for _, s := range broker.sessions {
		held = s
		break
	}
	broker.mu.Unlock()
	if held == nil {
		t.Fatal("expected the previously opened session")
	}
	broker.lazyCleanupWait = 10 * time.Millisecond
	unlock, err := broker.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", held.public.ViewerID, held.clientID))
	if err != nil {
		t.Fatal(err)
	}
	if !broker.reserveLazyCleanup() {
		t.Fatal("could not reserve cleanup")
	}
	internal := issuer.(*lanLazyStreams).lazy
	access := frontend.SessionAccess{Viewer: &core.Viewer{ID: 1}, Device: held.public.Device, Protocol: "dlna"}
	if !internal.discardOpened(ctx, access, held.public.ID) {
		unlock()
		t.Fatal("held gate did not schedule retry")
	}
	broker.mu.Lock()
	done := broker.lazyCleanupDone
	broker.mu.Unlock()
	unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("retry worker did not finish")
	}
	if len(pb.Sessions()) != 0 {
		t.Fatal("retry failed to clean up source")
	}
	broker.mu.Lock()
	reserved := broker.lazyCleanupReserved
	broker.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("cleanup reservation leaked: %d", reserved)
	}
	w = httptest.NewRecorder()
	pb.started, pb.finish = make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		issuer.ServeHTTP(w, request(http.MethodGet, newPath, "192.168.1.3"))
		close(finished)
	}()
	select {
	case <-pb.started:
	case <-time.After(3 * time.Second):
		close(pb.finish)
		t.Fatal("new session did not begin serving")
	}
	broker.mu.Lock()
	reserved = broker.lazyCleanupReserved
	broker.mu.Unlock()
	close(pb.finish)
	<-finished
	if reserved != 0 {
		t.Fatalf("active transfer held rollback reservation: %d", reserved)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("new session: %d", w.Code)
	}
	broker.mu.Lock()
	held = nil
	for _, s := range broker.sessions {
		held = s
		break
	}
	broker.mu.Unlock()
	if held == nil {
		t.Fatal("missing new session")
	}
	unlock, err = broker.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", held.public.ViewerID, held.clientID))
	if err != nil || !broker.reserveLazyCleanup() {
		t.Fatalf("cannot hold cleanup gate: %v", err)
	}
	access.Device = held.public.Device
	if !internal.discardOpened(ctx, access, held.public.ID) {
		unlock()
		t.Fatal("shutdown retry was not queued")
	}
	broker.mu.Lock()
	done = broker.lazyCleanupDone
	broker.mu.Unlock()
	closed := make(chan struct{})
	go func() { broker.Close(ctx); close(closed) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		unlock()
		t.Fatal("shutdown did not cancel cleanup worker")
	}
	unlock()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if len(pb.Sessions()) != 0 {
		t.Fatal("shutdown failed to drain session")
	}
}
