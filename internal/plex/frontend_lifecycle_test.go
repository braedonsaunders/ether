package plex

import (
	"context"
	"errors"
	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFrontendWatchSubscriptionFollowsMount(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	watch := state.New(store.OpenTest(t), state.Options{})
	defer watch.Close(ctx)
	adapter := Frontend{Server: f.srv, WatchState: watch}
	sub, ok := f.srv.notifier.subscribe([]string{"timeline"}, &ownerUser)
	if !ok {
		t.Fatal("notification subscription failed")
	}
	defer f.srv.notifier.unsubscribe(sub)
	change := func(surface core.Protocol, want bool) {
		t.Helper()
		if err := watch.Scrobble(state.WithOrigin(ctx, state.Origin{Surface: surface}), ownerUser.ID, core.MovieKey(5)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-sub.ch:
			if !want {
				t.Fatal("unexpected refresh while disabled or from Plex")
			}
		default:
			if want {
				t.Fatal("missing cross-surface refresh")
			}
		}
		select {
		case <-sub.ch:
			t.Fatal("duplicate refresh")
		default:
		}
	}
	change("jellyfin", false)
	for range 2 {
		instance, err := adapter.Mount(ctx, frontend.Host{})
		if err != nil {
			t.Fatal(err)
		}
		change("plex", false)
		change("jellyfin", true)
		if err := instance.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := instance.Close(ctx); err != nil {
			t.Fatal(err)
		}
		change("emby", false)
	}
}

type mountSessions struct{ frontend.Sessions }
type heldArtworkTransport struct{ started, release chan struct{} }

func (t heldArtworkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	close(t.started)
	<-t.release
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("held-artwork"))}, nil
}

func TestFrontendRetainsActiveRequestSessionAndImmutableHostOnRemount(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	firstHost, secondHost := &mountSessions{}, &mountSessions{}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRequest()
	f.srv.images = artworkservice.NewRemoteCache(1024, &http.Client{Transport: heldArtworkTransport{started, release}})
	adapter := Frontend{Server: f.srv}
	first, err := adapter.Mount(ctx, frontend.Host{Sessions: firstHost})
	if err != nil {
		t.Fatal(err)
	}
	// Preserve a currently playing protocol session across a routing toggle.
	id := trackID(ownerUser.ID, "client", "active")
	f.srv.sessions.ensure(id, 17)
	f.srv.sessions.update(id, func(e *playEntry) {
		e.user = ownerUser
		e.key = fightClub.Key
		e.state = "playing"
		e.updatedAt = time.Now()
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/library/metadata/"+fightClub.Key.String()+"/poster", nil)
	request.Header.Set("X-Plex-Token", ownerToken)
	done := make(chan struct{})
	go func() { first.Handler().ServeHTTP(response, request); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		releaseRequest()
		t.Fatal("request did not reach image source")
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if f.srv.ctx.Err() != nil || !f.srv.sessions.has(id) {
		t.Fatal("unmount destroyed retained server/session")
	}
	second, err := adapter.Mount(ctx, frontend.Host{Sessions: secondHost})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(ctx)
	oldBroker, err := first.(*plexInstance).sessionBroker()
	if err != nil || oldBroker != firstHost {
		t.Fatal("old mount scope changed")
	}
	newBroker, err := second.(*plexInstance).sessionBroker()
	if err != nil || newBroker != secondHost {
		t.Fatal("new mount scope missing")
	}
	if first.(*plexInstance).server != second.(*plexInstance).server {
		t.Fatal("remount replaced active server")
	}
	releaseRequest()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("held request did not finish")
	}
	if response.Code != 200 || response.Body.String() != "held-artwork" {
		t.Fatalf("held request: %d %q", response.Code, response.Body.String())
	}
	if _, err := (&plexInstance{}).sessionBroker(); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("nil host was not fail closed: %v", err)
	}
}

func TestFrontendCancelledMountDoesNotConstructOrSubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	adapter := Frontend{NewServer: func() (*Server, error) { called = true; return nil, nil }}
	if _, err := adapter.Mount(ctx, frontend.Host{}); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled mount called factory=%v err=%v", called, err)
	}
}
