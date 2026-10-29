package plex

import (
	"context"
	"errors"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// Frontend adapts the existing PMS handler to the frontend registry. The app
// owns the server's lifetime because Plex-only integrations still reference it;
// unmounting removes its handler without replacing its playback callbacks.
type Frontend struct {
	Server        *Server
	StatusFunc    func(context.Context) frontend.Status
	ListenersFunc func() []frontend.ListenerSpec
}

func (f Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "plex", Name: "Plex", DefaultPort: 32400}
}
func (f Frontend) Mount(_ context.Context, _ frontend.Host) (frontend.Instance, error) {
	if f.Server == nil {
		return nil, errors.New("plex: frontend server is unavailable")
	}
	return plexInstance{server: f.Server, status: f.StatusFunc, listeners: f.ListenersFunc}, nil
}

type plexInstance struct {
	server    *Server
	status    func(context.Context) frontend.Status
	listeners func() []frontend.ListenerSpec
}

func (i plexInstance) Handler() http.Handler { return i.server }

// Plex shares the existing owner-console listener; the app owns that socket.
func (i plexInstance) Listeners() []frontend.ListenerSpec {
	if i.listeners != nil {
		return i.listeners()
	}
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/"}}
}
func (i plexInstance) Discovery() []frontend.Announcer { return nil }
func (i plexInstance) Status(ctx context.Context) frontend.Status {
	if i.status != nil {
		return i.status(ctx)
	}
	return frontend.Status{}
}
func (i plexInstance) Close(context.Context) error { return nil }

var _ frontend.Frontend = Frontend{}
