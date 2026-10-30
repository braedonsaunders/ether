package sessions

import (
	"context"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// BindSurface prevents an adapter from submitting another surface's identity.
func BindSurface(b frontend.Sessions, protocol frontend.Protocol) frontend.Sessions {
	return surface{broker: b, protocol: protocol}
}

type surface struct {
	broker   frontend.Sessions
	protocol frontend.Protocol
}

// BindEvents applies the originating surface filter to user-data events.
func BindEvents(events frontend.Subscriber, protocol frontend.Protocol) frontend.Subscriber {
	return surfaceEvents{events: events, protocol: protocol}
}

type surfaceEvents struct {
	events   frontend.Subscriber
	protocol frontend.Protocol
}

func (s surfaceEvents) Subscribe(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	scope.Protocol = s.protocol
	return s.events.Subscribe(ctx, scope)
}

func (s surface) Plan(ctx context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	if r.Protocol != s.protocol {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	return s.broker.Plan(ctx, r)
}
func (s surface) DetailPlan(ctx context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	if r.Protocol != s.protocol {
		return frontend.PlayPlan{}, core.ErrUnauthorized
	}
	if planner, ok := s.broker.(frontend.DetailPlanner); ok {
		return planner.DetailPlan(ctx, r)
	}
	return frontend.PlayPlan{}, core.ErrNotAvailable
}
func (s surface) ResolveDetailSource(ctx context.Context, scope frontend.VersionScope, handle string) (frontend.DetailSourceTarget, error) {
	if scope.Protocol != s.protocol {
		return frontend.DetailSourceTarget{}, core.ErrUnauthorized
	}
	if planner, ok := s.broker.(frontend.DetailPlanner); ok {
		return planner.ResolveDetailSource(ctx, scope, handle)
	}
	return frontend.DetailSourceTarget{}, core.ErrNotAvailable
}
func (s surface) MatchDetailSession(ctx context.Context, scope frontend.VersionScope, handle, sessionID string) error {
	if scope.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	if planner, ok := s.broker.(frontend.DetailPlanner); ok {
		return planner.MatchDetailSession(ctx, scope, handle, sessionID)
	}
	return core.ErrNotAvailable
}
func (s surface) Open(ctx context.Context, r frontend.OpenRequest) (frontend.Session, error) {
	if r.Protocol != s.protocol {
		return frontend.Session{}, core.ErrUnauthorized
	}
	return s.broker.Open(ctx, r)
}
func (s surface) StartStream(ctx context.Context, a frontend.SessionAccess, id string, r frontend.StreamRequest) (string, error) {
	if a.Protocol != s.protocol {
		return "", core.ErrUnauthorized
	}
	return s.broker.StartStream(ctx, a, id, r)
}
func (s surface) SubtitleURL(ctx context.Context, a frontend.SessionAccess, id string, index int, format string) (string, error) {
	if a.Protocol != s.protocol {
		return "", core.ErrUnauthorized
	}
	return s.broker.SubtitleURL(ctx, a, id, index, format)
}
func (s surface) ServeMedia(w http.ResponseWriter, r *http.Request) {
	if scoped, ok := s.broker.(interface {
		ServeMediaFor(frontend.Protocol, http.ResponseWriter, *http.Request)
	}); ok {
		scoped.ServeMediaFor(s.protocol, w, r)
		return
	}
	http.NotFound(w, r)
}
func (s surface) ServeMediaAccess(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess) {
	if a.Protocol != s.protocol {
		http.NotFound(w, r)
		return
	}
	if scoped, ok := s.broker.(frontend.ScopedMedia); ok {
		scoped.ServeMediaAccess(w, r, a)
		return
	}
	http.NotFound(w, r)
}
func (s surface) Report(ctx context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	if a.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	return s.broker.Report(ctx, a, id, t)
}
func (s surface) Stop(ctx context.Context, a frontend.SessionAccess, id string) error {
	if a.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	return s.broker.Stop(ctx, a, id)
}
func (s surface) Command(ctx context.Context, actor *core.Viewer, id string, cmd frontend.Command) error {
	return s.broker.Command(ctx, actor, id, cmd)
}
func (s surface) UpdateCapabilities(ctx context.Context, a frontend.SessionAccess, c frontend.SessionCapabilities) error {
	if a.Protocol != s.protocol {
		return core.ErrUnauthorized
	}
	return s.broker.UpdateCapabilities(ctx, a, c)
}
func (s surface) Capabilities(ctx context.Context, a frontend.SessionAccess) (frontend.SessionCapabilities, error) {
	if a.Protocol != s.protocol {
		return frontend.SessionCapabilities{}, core.ErrUnauthorized
	}
	return s.broker.Capabilities(ctx, a)
}
func (s surface) Sessions(ctx context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	if a.Protocol != s.protocol {
		return nil, core.ErrUnauthorized
	}
	return s.broker.Sessions(ctx, a)
}
func (s surface) Recover(ctx context.Context, id, url string, at time.Duration) error {
	return s.broker.Recover(ctx, id, url, at)
}
func (s surface) TranscodeFor(ctx context.Context, id string) (core.HLSSession, bool) {
	return s.broker.TranscodeFor(ctx, id)
}
func (s surface) Events(ctx context.Context, scope frontend.EventScope) (<-chan frontend.Event, error) {
	scope.Protocol = s.protocol
	return s.broker.Events(ctx, scope)
}
