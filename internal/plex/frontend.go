package plex

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
)

// Frontend adapts the existing PMS handler to the frontend registry. The app
// owns the server's lifetime because Plex-only integrations still reference it;
// unmounting removes its handler without replacing its playback callbacks.
type Frontend struct {
	NewServer  func() (*Server, error)
	Server     *Server
	WatchState *state.State
	// ExplicitWatchOnly preserves spike mode: only cross-surface scrobbles refresh.
	ExplicitWatchOnly bool
	StatusFunc        func(context.Context) frontend.Status
	ListenersFunc     func() []frontend.ListenerSpec
}

func (f Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "plex", Name: "Plex", DefaultPort: 32400}
}
func (f Frontend) Mount(ctx context.Context, host frontend.Host) (frontend.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.Server == nil && f.NewServer != nil {
		var err error
		f.Server, err = f.NewServer()
		if err != nil {
			return nil, err
		}
	}
	if f.Server == nil {
		return nil, errors.New("plex: frontend server is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	instance := &plexInstance{sessions: host.Sessions, server: f.Server, status: f.StatusFunc, listeners: f.ListenersFunc}
	if f.WatchState != nil {
		instance.unsubscribe = subscribeWatchRefresh(f.WatchState, f.Server.WatchStateChanged, f.ExplicitWatchOnly)
	}
	return instance, nil
}

type plexInstance struct {
	server *Server
	// sessions is immutable per mount; never install it on the retained Server.
	sessions    frontend.Sessions
	unsubscribe func()
	closeOnce   sync.Once
	status      func(context.Context) frontend.Status
	listeners   func() []frontend.ListenerSpec
}

func (i *plexInstance) Handler() http.Handler { return i.server }

// Plex shares the existing owner-console listener; the app owns that socket.
func (i *plexInstance) Listeners() []frontend.ListenerSpec {
	if i.listeners != nil {
		return i.listeners()
	}
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/"}}
}
func (i *plexInstance) Discovery() []frontend.Announcer { return nil }
func (i *plexInstance) Status(ctx context.Context) frontend.Status {
	if i.status != nil {
		return i.status(ctx)
	}
	return frontend.Status{}
}
func (i *plexInstance) Close(context.Context) error {
	i.closeOnce.Do(func() {
		if i.unsubscribe != nil {
			i.unsubscribe()
		}
	})
	return nil
}

var _ frontend.Frontend = Frontend{}

// sessionBroker is the fail-closed seam for future migrated handlers. Capturing
// the host here does not route existing Plex traffic through the shared broker.
func (i *plexInstance) sessionBroker() (frontend.Sessions, error) {
	if i.sessions == nil {
		return nil, core.ErrNotAvailable
	}
	return i.sessions, nil
}
