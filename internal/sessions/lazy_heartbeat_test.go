package sessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type heartbeatLazyPlayback struct {
	*fakePlayback
	once      sync.Once
	started   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
}

func (p *heartbeatLazyPlayback) ServeRange(w http.ResponseWriter, r *http.Request, _ string) {
	blocked := false
	p.once.Do(func() { blocked = true; close(p.started) })
	if blocked {
		select {
		case <-p.release:
		case <-r.Context().Done():
			close(p.cancelled)
			return
		}
	}
	_, _ = w.Write([]byte("range"))
}

func TestLazyLongTransferHeartbeat(t *testing.T) {
	for _, ending := range []string{"retry", "revoke", "database", "stuck"} {
		t.Run(ending, func(t *testing.T) {
			ctx := context.Background()
			db := store.OpenTest(t)
			var clock atomic.Int64
			clock.Store(time.Now().UnixMilli())
			now := func() time.Time { return time.UnixMilli(clock.Load()) }
			var blockActive atomic.Bool
			activeEntered := make(chan struct{})
			activeRelease := make(chan struct{})
			active := func(_ context.Context, id int64) (*core.Viewer, error) {
				if blockActive.Load() {
					close(activeEntered)
					<-activeRelease // Deliberately ignores cancellation.
				}
				return &core.Viewer{ID: id}, nil
			}
			events := NewEventBus(nil, active)
			defer events.Close()
			pb := &heartbeatLazyPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, started: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan struct{})}
			lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: key}, nil
			}
			b, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Now: now, Lookup: lookup, MediaLookup: lookup})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close(ctx)
			grants := &fakeLazyGrants{}
			iface, err := NewLazyStreams(db, b, "stremio", grants, time.Hour, func(*http.Request) bool { return false })
			if err != nil {
				t.Fatal(err)
			}
			lazy := iface.(*lazyStreams)
			ticks := make(chan time.Time)
			stopped := make(chan struct{})
			lazy.newHeartbeat = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() { close(stopped) } }
			req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "long"}, Protocol: "stremio", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
			path, err := lazy.Issue(ctx, "account-credential-secret", req)
			if err != nil {
				t.Fatal(err)
			}
			if ending == "retry" {
				for n := 0; n < maxLazyHeartbeats; n++ {
					if !b.reserveLazyHeartbeat() {
						t.Fatal("heartbeat capacity unavailable early")
					}
				}
				w := httptest.NewRecorder()
				lazy.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				if w.Code != http.StatusServiceUnavailable || pb.prepares != 0 {
					t.Fatalf("saturated status=%d prepares=%d", w.Code, pb.prepares)
				}
				for n := 0; n < maxLazyHeartbeats; n++ {
					b.releaseLazyHeartbeat()
				}
			}
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				lazy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
			}()
			select {
			case <-pb.started:
			case <-time.After(5 * time.Second):
				t.Fatal("transfer did not start")
			}
			hash := lazyHash(path[len("/sessions/lazy/") : len(path)-len("/file")])
			for n := 0; n < 7; n++ {
				clock.Add(int64((30 * time.Minute) / time.Millisecond))
				select {
				case ticks <- now():
				case <-time.After(5 * time.Second):
					t.Fatal("heartbeat stopped")
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					var expiry int64
					if err := db.QueryRow(`SELECT expires FROM sessions_lazy_streams WHERE hash=?`, hash).Scan(&expiry); err == nil && expiry >= now().Add(time.Hour).UnixMilli() {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("heartbeat did not renew")
					}
					time.Sleep(time.Millisecond)
				}
			}
			switch ending {
			case "retry":
				close(pb.release)
				<-finished
				<-stopped
				lazy.newHeartbeat = nil
				w := httptest.NewRecorder()
				lazy.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				if w.Code != http.StatusOK || pb.prepares != 1 {
					t.Fatalf("retry=%d prepares=%d", w.Code, pb.prepares)
				}
			case "revoke":
				grants.mu.Lock()
				grants.revoked = true
				grants.mu.Unlock()
				ticks <- now()
				select {
				case <-pb.cancelled:
				case <-time.After(5 * time.Second):
					t.Fatal("revocation did not cancel transfer")
				}
				<-finished
				<-stopped
			case "database":
				if _, err := db.Exec(`DROP TABLE sessions_lazy_streams`); err != nil {
					t.Fatal(err)
				}
				ticks <- now()
				select {
				case <-pb.cancelled:
				case <-time.After(5 * time.Second):
					t.Fatal("database failure did not cancel transfer")
				}
				<-finished
				<-stopped
			case "stuck":
				blockActive.Store(true)
				ticks <- now()
				<-activeEntered
				close(pb.release)
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Fatal("uncancellable dependency blocked request teardown")
				}
				b.mu.Lock()
				remaining := b.lazyHeartbeatCount
				b.mu.Unlock()
				if remaining != 1 {
					t.Fatalf("stuck worker reservation=%d", remaining)
				}
				blockActive.Store(false)
				close(activeRelease)
				<-stopped
			}
		})
	}
}
