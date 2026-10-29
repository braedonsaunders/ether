package sessions

import (
	"context"
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
	mu      sync.Mutex
	revoked bool
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
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: func() time.Time { return now }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	grants := &fakeLazyGrants{}
	lazy, err := NewLazyStreams(db, b, "stremio", grants, time.Hour)
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
	if head.Code == http.StatusOK || pb.prepares != 0 {
		t.Fatal("HEAD opened playback")
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
	// A fresh surface handler can redeem the durable capability from the DB.
	restarted, err := NewLazyStreams(db, b, "stremio", grants, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reused := httptest.NewRecorder()
	restarted.ServeHTTP(reused, httptest.NewRequest(http.MethodGet, path, nil))
	if reused.Code != http.StatusOK {
		t.Fatalf("durable capability status %d", reused.Code)
	}
	var serialized string
	if err := db.QueryRow(`SELECT request FROM sessions_lazy_streams LIMIT 1`).Scan(&serialized); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(serialized, "account-credential-secret") || strings.Contains(serialized, "grant-reference") {
		t.Fatal("credential or grant reference in serialized playback request")
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
	grants.mu.Unlock()
	now = now.Add(time.Hour)
	expired := httptest.NewRecorder()
	lazy.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, path, nil))
	if expired.Code != http.StatusNotFound {
		t.Fatalf("expired status %d", expired.Code)
	}
}
