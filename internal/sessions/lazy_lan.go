package sessions

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type lanRequestKey struct{}

type lanGrants struct {
	clientIP func(*http.Request) (netip.Addr, bool)
	viewer   func(context.Context, *http.Request) (*core.Viewer, error)
}

func (g lanGrants) current(ctx context.Context) (*core.Viewer, core.Device, error) {
	r, ok := ctx.Value(lanRequestKey{}).(*http.Request)
	if !ok || r == nil {
		return nil, core.Device{}, core.ErrUnauthorized
	}
	peer, ok := g.clientIP(r)
	if !ok || !peer.IsValid() {
		return nil, core.Device{}, core.ErrUnauthorized
	}
	viewer, err := g.viewer(ctx, r)
	if err != nil || viewer == nil || viewer.ID <= 0 {
		return nil, core.Device{}, core.ErrUnauthorized
	}
	return viewer, frontend.LANDevice(peer), nil
}

func (g lanGrants) GrantReference(ctx context.Context, surface, credential string) (string, *core.Viewer, error) {
	if surface != "dlna" || credential != "lan" {
		return "", nil, core.ErrUnauthorized
	}
	viewer, device, err := g.current(ctx)
	if err != nil || device.ID == "" {
		return "", nil, core.ErrUnauthorized
	}
	return device.ID, viewer, nil
}

func (g lanGrants) ValidateGrantReference(ctx context.Context, surface, reference string, viewerID int64) error {
	if surface != "dlna" {
		return core.ErrUnauthorized
	}
	viewer, device, err := g.current(ctx)
	if err != nil || viewer.ID != viewerID || device.ID == "" || device.ID != reference {
		return core.ErrUnauthorized
	}
	return nil
}

type lanLazyStreams struct {
	lazy   *lazyStreams
	grants lanGrants
}

// NewLANLazyStreams derives authority from host-owned peer and viewer checks.
// Capabilities are bound to that viewer, canonical peer device and item.
func NewLANLazyStreams(db *sql.DB, broker *Broker, clientIP func(*http.Request) (netip.Addr, bool), viewer func(context.Context, *http.Request) (*core.Viewer, error), ttl time.Duration) (frontend.LANLazyStreams, error) {
	if clientIP == nil || viewer == nil {
		return nil, errors.New("sessions: missing LAN authority")
	}
	grants := lanGrants{clientIP: clientIP, viewer: viewer}
	streams, err := NewLazyStreams(db, broker, "dlna", grants, ttl, func(*http.Request) bool { return false })
	if err != nil {
		return nil, err
	}
	return &lanLazyStreams{lazy: streams.(*lazyStreams), grants: grants}, nil
}

func (s *lanLazyStreams) IssueLAN(ctx context.Context, r *http.Request, req frontend.PlanRequest) (string, error) {
	if r == nil || req.Protocol != "dlna" || req.Item == 0 || req.Channel != "" {
		return "", core.ErrUnauthorized
	}
	ctx = context.WithValue(ctx, lanRequestKey{}, r)
	viewer, device, err := s.grants.current(ctx)
	if err != nil || (req.Viewer != nil && req.Viewer.ID != viewer.ID) || (req.Device.ID != "" && req.Device.ID != device.ID) {
		return "", core.ErrUnauthorized
	}
	req.Viewer, req.Device = viewer, device
	req.Caps.Remote = false
	return s.lazy.Issue(ctx, "lan", req)
}

func (s *lanLazyStreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), lanRequestKey{}, r)
	s.lazy.ServeHTTP(w, r.WithContext(ctx))
}
