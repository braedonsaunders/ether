package sessions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeLazyGrants struct {
	mu       sync.Mutex
	revoked  bool
	checks   int
	revokeAt int
}

func (g *fakeLazyGrants) GrantReference(_ context.Context, surface, credential string) (string, *core.Viewer, error) {
	if surface != "stremio" || credential != "account-credential-secret" {
		return "", nil, core.ErrUnauthorized
	}
	return "grant-reference", &core.Viewer{ID: 1}, nil
}
func (g *fakeLazyGrants) ValidateGrantReference(_ context.Context, surface, ref string, viewer int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks++
	if g.revokeAt > 0 && g.checks >= g.revokeAt {
		g.revoked = true
	}
	if g.revoked || surface != "stremio" || ref != "grant-reference" || viewer != 1 {
		return core.ErrUnauthorized
	}
	return nil
}
func TestLazyStreamCapabilityReusesSessionAndRevalidatesGrant(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 3}
	networkLookups := 0
	b, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(_ *core.Viewer, remote bool, _ time.Time) bool { return !remote }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: func() time.Time { return now }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		networkLookups++
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	mediaHidden := false
	b.deps.MediaLookup = func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		if mediaHidden {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: k}, nil
	}
	defer b.Close(ctx)
	grants := &fakeLazyGrants{}
	lazy, err := NewLazyStreams(db, b, "stremio", grants, time.Hour, func(r *http.Request) bool { return r.Header.Get("X-Test-Remote") == "yes" })
	if err != nil {
		t.Fatal(err)
	}
	req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "install-device"}, Protocol: "stremio", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
	path, err := lazy.Issue(ctx, "account-credential-secret", req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "account-credential-secret") || strings.Contains(path, "grant-reference") || pb.prepares != 0 {
		t.Fatalf("issue resolved or leaked: %q prepares=%d", path, pb.prepares)
	}
	variantCalls := 0
	b.deps.Variants = func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
		variantCalls++
		return nil, core.ErrNotAvailable
	}
	variantRequest := req
	variantRequest.Version = "chosen-version"
	if _, err := lazy.Issue(ctx, "account-credential-secret", variantRequest); err != nil || variantCalls != 0 {
		t.Fatalf("issue resolved variant: calls=%d err=%v", variantCalls, err)
	}
	head := httptest.NewRecorder()
	lazy.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))
	if head.Code != http.StatusOK || pb.prepares != 0 {
		t.Fatalf("HEAD status=%d, prepares=%d", head.Code, pb.prepares)
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Range", "bytes=0-2")
			lazy.ServeHTTP(out, r)
			statuses <- out.Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("range status %d", status)
		}
	}
	if pb.prepares != 1 || len(pb.Sessions()) != 1 {
		t.Fatalf("opens %d sessions %d", pb.prepares, len(pb.Sessions()))
	}
	remoteRequest := httptest.NewRequest(http.MethodGet, path, nil)
	remoteRequest.Header.Set("X-Test-Remote", "yes")
	remoteResponse := httptest.NewRecorder()
	lazy.ServeHTTP(remoteResponse, remoteRequest)
	if remoteResponse.Code != http.StatusNotFound {
		t.Fatalf("remote policy bypass: %d", remoteResponse.Code)
	}
	// A fresh surface handler can redeem the durable capability from the DB.
	restarted, err := NewLazyStreams(db, b, "stremio", grants, time.Hour, func(r *http.Request) bool { return r.Header.Get("X-Test-Remote") == "yes" })
	if err != nil {
		t.Fatal(err)
	}
	reused := httptest.NewRecorder()
	restarted.ServeHTTP(reused, httptest.NewRequest(http.MethodGet, path, nil))
	if reused.Code != http.StatusOK {
		t.Fatalf("durable capability status %d", reused.Code)
	}
	if pb.prepares != 1 || len(pb.Sessions()) != 1 {
		t.Fatalf("handler recreation opened another session: %d", pb.prepares)
	}
	var serialized string
	if err := db.QueryRow(`SELECT request FROM sessions_lazy_streams LIMIT 1`).Scan(&serialized); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(serialized, "account-credential-secret") || strings.Contains(serialized, "grant-reference") {
		t.Fatal("credential or grant reference in serialized playback request")
	}
	authenticated := req
	authenticated.Device.ID = "webdav-device"
	var firstLookups int
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil)
		r.Header.Set("Range", "bytes=0-2")
		current := authenticated
		if i == 1 {
			current.Device.Client = "renamed client"
			current.Device.DeviceName = "renamed device"
			current.Caps.DeviceName = "renamed device"
		}
		lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, r, "account-credential-secret", current)
		if w.Code != http.StatusOK {
			t.Fatalf("authenticated range %d: %d", i, w.Code)
		}
		if i == 0 {
			firstLookups = networkLookups
		} else if networkLookups != firstLookups {
			t.Fatal("reused authenticated range fetched network metadata")
		}
	}
	if pb.prepares != 2 || len(pb.Sessions()) != 2 {
		t.Fatalf("authenticated ranges did not reuse a session: prepares=%d", pb.prepares)
	}
	changed := authenticated
	changed.Caps.MaxHeight = 720
	deniedDecision := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(deniedDecision, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", changed)
	if deniedDecision.Code != http.StatusNotFound || pb.prepares != 2 {
		t.Fatalf("changed decision was reused: status=%d prepares=%d", deniedDecision.Code, pb.prepares)
	}
	var concurrent sync.WaitGroup
	statuses = make(chan int, 4)
	for n := 0; n < 4; n++ {
		concurrent.Add(1)
		go func() {
			defer concurrent.Done()
			w := httptest.NewRecorder()
			lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
			statuses <- w.Code
		}()
	}
	concurrent.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent authenticated range: %d", status)
		}
	}
	if pb.prepares != 2 || len(pb.Sessions()) != 2 {
		t.Fatalf("concurrent ranges opened another session: %d", pb.prepares)
	}
	foreign := authenticated
	foreign.Viewer = &core.Viewer{ID: 2}
	for _, tc := range []struct {
		credential string
		request    frontend.PlanRequest
	}{{"invalid", authenticated}, {"account-credential-secret", foreign}} {
		w := httptest.NewRecorder()
		lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), tc.credential, tc.request)
		if w.Code != http.StatusNotFound {
			t.Fatalf("foreign credential/viewer accepted: %d", w.Code)
		}
	}
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	revokedRange := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(revokedRange, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
	if revokedRange.Code != http.StatusNotFound || len(pb.Sessions()) != 2 {
		t.Fatalf("revoked range status=%d sessions=%d", revokedRange.Code, len(pb.Sessions()))
	}
	grants.mu.Lock()
	grants.revoked = false
	grants.mu.Unlock()
	mediaHidden = true
	hiddenRange := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(hiddenRange, httptest.NewRequest(http.MethodGet, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
	if hiddenRange.Code != http.StatusNotFound || len(pb.Sessions()) != 2 {
		t.Fatalf("hidden item status=%d sessions=%d", hiddenRange.Code, len(pb.Sessions()))
	}
	mediaHidden = false
	headAuth := httptest.NewRecorder()
	lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(headAuth, httptest.NewRequest(http.MethodHead, "/dav/movie.mkv", nil), "account-credential-secret", authenticated)
	if headAuth.Code == http.StatusOK || pb.prepares != 2 {
		t.Fatal("authenticated HEAD opened playback")
	}
	other := req
	other.Device.ID = "other-install"
	secondPath, err := lazy.Issue(ctx, "account-credential-secret", other)
	if err != nil {
		t.Fatal(err)
	}
	grants.mu.Lock()
	grants.checks = 0
	grants.revokeAt = 3
	grants.mu.Unlock()
	midOpen := httptest.NewRecorder()
	lazy.ServeHTTP(midOpen, httptest.NewRequest(http.MethodGet, secondPath, nil))
	if midOpen.Code != http.StatusNotFound || len(pb.Sessions()) != 2 {
		t.Fatalf("revocation during open: status=%d sessions=%d", midOpen.Code, len(pb.Sessions()))
	}
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	denied := httptest.NewRecorder()
	lazy.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, path, nil))
	if denied.Code != http.StatusNotFound {
		t.Fatalf("revoked status %d", denied.Code)
	}
	grants.mu.Lock()
	grants.revoked = false
	grants.revokeAt = 0
	grants.mu.Unlock()
	now = now.Add(time.Hour)
	expired := httptest.NewRecorder()
	lazy.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, path, nil))
	if expired.Code != http.StatusNotFound {
		t.Fatalf("expired status %d", expired.Code)
	}
}

func TestAuthenticatedLazyConcurrentFirstRangesReuseSession(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	broker, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	lazy, err := NewLazyStreams(db, broker, "stremio", &fakeLazyGrants{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	request := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "webdav-device"}, Protocol: "stremio", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			lazy.(frontend.AuthenticatedLazyStreams).ServeAuthenticated(w, httptest.NewRequest(http.MethodGet, "/movie", nil), "account-credential-secret", request)
			statuses <- w.Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent status: %d", status)
		}
	}
	if pb.prepares != 1 || len(pb.Sessions()) != 1 {
		t.Fatalf("prepared=%d sessions=%d", pb.prepares, len(pb.Sessions()))
	}
}

func TestAuthenticatedLazyConflictAndExpiry(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Now()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	broker, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	iface, err := NewLazyStreams(db, broker, "stremio", &fakeLazyGrants{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	lazy := iface.(*lazyStreams)
	first := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "same-device"}, Protocol: "stremio", Item: core.MovieKey(1)}
	second := first
	second.Caps.MaxHeight = 720
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, request := range []frontend.PlanRequest{first, second} {
		go func(r frontend.PlanRequest) {
			<-start
			_, err := lazy.issue(ctx, "account-credential-secret", r, true)
			results <- err
		}(request)
	}
	close(start)
	firstErr, secondErr := <-results, <-results
	if (firstErr == nil) == (secondErr == nil) {
		t.Fatalf("conflicting decisions: %v, %v", firstErr, secondErr)
	}
	if firstErr != nil && !errors.Is(firstErr, core.ErrNotAvailable) || secondErr != nil && !errors.Is(secondErr, core.ErrNotAvailable) {
		t.Fatalf("conflict failed unexpectedly: %v, %v", firstErr, secondErr)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
	now = now.Add(time.Hour)
	if _, err := lazy.issue(ctx, "account-credential-secret", second, true); err != nil {
		t.Fatalf("replacement after expiry: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expiry cleanup rows=%d err=%v", count, err)
	}
	if pb.prepares != 0 {
		t.Fatal("issuing or expiring capabilities opened playback")
	}
}
