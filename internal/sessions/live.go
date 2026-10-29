package sessions

import (
	"context"
	"crypto/rand"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// LiveBackend adapts the existing channel playout service. Serve receives a
// grant-validated request and a path relative to the scoped live stream.
type LiveBackend interface {
	Allowed(context.Context, *core.Viewer, string) error
	Tune(context.Context, *core.Viewer, string, core.ClientCaps, string) (string, error)
	Serve(http.ResponseWriter, *http.Request, *core.Viewer, string, string, string)
	Stop(context.Context, *core.Viewer, string) error
}

func (b *Broker) planLive(ctx context.Context, req frontend.PlanRequest, viewer *core.Viewer) (frontend.PlayPlan, error) {
	if b.deps.Live == nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if err := b.deps.Live.Allowed(ctx, viewer, req.Channel); err != nil {
		return frontend.PlayPlan{}, err
	}
	if req.Intent.AllowTranscoding != nil && !*req.Intent.AllowTranscoding {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	info := core.MediaInfo{Container: "mpegts", Estimated: true}
	decision := core.Decision{Kind: core.DecisionRemux, Container: "mpegts"}
	if req.Intent.Estimate {
		return frontend.PlayPlan{Estimated: true, Media: info, Decision: decision}, nil
	}
	id := rand.Text()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.plans)+b.pendingPlans >= 1024 {
		return frontend.PlayPlan{}, core.ErrStreamLimit
	}
	for key, p := range b.plans {
		if b.deps.Now().Sub(p.created) > 2*time.Minute {
			delete(b.plans, key)
		}
	}
	b.plans[id] = brokerPlan{request: req, decision: decision, created: b.deps.Now(), live: true}
	return frontend.PlayPlan{ID: id, Media: info, Decision: decision}, nil
}

func (b *Broker) openLive(ctx context.Context, req frontend.OpenRequest, p brokerPlan, viewer *core.Viewer) (frontend.Session, error) {
	if b.deps.Live == nil {
		return frontend.Session{}, core.ErrNotAvailable
	}
	if err := b.deps.Live.Allowed(ctx, viewer, p.request.Channel); err != nil {
		return frontend.Session{}, err
	}
	liveID, err := b.deps.Live.Tune(ctx, viewer, p.request.Channel, p.request.Caps, p.request.Intent.ClientSessionID)
	if err != nil {
		return frontend.Session{}, err
	}
	id, grant := rand.Text(), rand.Text()
	session := frontend.Session{ID: id, MediaPath: "/sessions/media/" + id + "/" + grant + "/live/index.m3u8", Protocol: req.Protocol, ViewerID: viewer.ID, Device: p.request.Device, Channel: p.request.Channel, StartedAt: b.deps.Now()}
	s := &brokerSession{public: session, grant: grant, playID: liveID, clientID: p.request.Caps.ClientID, caps: p.request.Caps, opts: p.request.Intent.Options, decision: p.decision, live: true}
	if b.cluster != nil && session.Protocol != "plex" {
		if err := b.cluster.store.put(ctx, session, liveID, *viewer, b.deps.Now()); err != nil {
			_ = b.deps.Live.Stop(ctx, viewer, liveID)
			return frontend.Session{}, err
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		if b.cluster != nil {
			_ = b.cluster.store.remove(ctx, id)
		}
		_ = b.deps.Live.Stop(ctx, viewer, liveID)
		return frontend.Session{}, core.ErrNotAvailable
	}
	b.sessions[id] = s
	b.mu.Unlock()
	b.publish(ctx, sessionEvent("SessionStarted", session, nil))
	return session, nil
}
