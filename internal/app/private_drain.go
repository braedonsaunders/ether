package app

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/clusterplayback"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func (a *App) distributedHLS() sessions.DistributedHLS {
	if a.deployment == nil {
		return nil
	}
	runtime, _ := a.deployment.playback.(sessions.DistributedHLS)
	return runtime
}

type drainBarrierSnapshot struct {
	total, runtime, sessions int
	categories               *clusterconfig.DrainPendingCategories
}

func (a *App) privatePending(ctx context.Context) (int, error) {
	st, err := a.privatePendingSnapshot(ctx)
	return st.total, err
}

func (a *App) privatePendingSnapshot(ctx context.Context) (drainBarrierSnapshot, error) {
	st := drainBarrierSnapshot{total: -1, runtime: -1}
	if a.brokerPlayback != nil {
		st.sessions = len(a.brokerPlayback.Sessions())
	} else if a.playback != nil {
		st.sessions = len(a.playback.Sessions())
	}
	if a.deployment == nil {
		return st, errors.New("retirement accounting unavailable")
	}
	if rt, ok := a.deployment.playback.(interface {
		PendingDetails(context.Context) (clusterplayback.ResponsibilityCounts, error)
	}); ok {
		counts, err := rt.PendingDetails(ctx)
		if err != nil {
			return st, err
		}
		st.runtime = counts.Total()
		st.categories = &clusterconfig.DrainPendingCategories{
			SourcePreparations: counts.SourcePreparations, PreparationLifetimes: counts.PreparationLifetimes, PreparationReceipts: counts.PreparationReceipts,
			SourceLifetimes: counts.SourceLifetimes, SourceCallbacks: counts.SourceCallbacks,
			EncoderStarts: counts.EncoderStarts, EncoderTokens: counts.EncoderTokens, Inputs: counts.Inputs,
			RemotePreparing: counts.RemotePreparing, RemoteLive: counts.RemoteLive, RemoteRetiring: counts.RemoteRetiring,
			Executions: counts.Executions, Reopens: counts.Reopens, Placements: counts.Placements,
		}
	} else if rt, ok := a.deployment.playback.(interface {
		Pending(context.Context) (int, error)
	}); ok {
		n, err := rt.Pending(ctx)
		if err != nil {
			return st, err
		}
		st.runtime = n
	} else {
		return st, errors.New("retirement accounting unavailable")
	}
	if st.runtime < 0 {
		return drainBarrierSnapshot{total: -1, runtime: -1, sessions: st.sessions}, errors.New("retirement accounting unconfirmed")
	}
	st.total = st.runtime + st.sessions
	return st, nil
}
func (a *App) pollDrain(ctx context.Context) {
	req, err := clusterconfig.ReadDrainControl(a.opts.DataDir, clusterconfig.DrainRequestFile)
	if err != nil || req.Boot != a.cluster.Self().Incarnation || req.Operation == "" || req.State != "requested" {
		return
	}
	a.draining.Store(true)
	if remote, ok := a.brokerPlayback.(interface{ SetDraining(bool) }); ok {
		remote.SetDraining(true)
	}
	if a.playback != nil {
		a.playback.SetDraining(true)
	}
	st, err := a.privatePendingSnapshot(ctx)
	req.Pending = st.total
	req.PendingRuntime, req.PendingCategories = nil, nil
	req.PendingSessions = &st.sessions
	if err == nil {
		req.PendingRuntime, req.PendingCategories = &st.runtime, st.categories
	}
	req.State = "draining"
	if err == nil && st.total == 0 {
		req.State = "drained"
	}
	_ = clusterconfig.WriteDrainControl(a.opts.DataDir, clusterconfig.DrainStatusFile, req)
}

// drainPrivate keeps membership, TLS, existing-media handlers and independent
// Tick/cleanup alive. Deadline ends waiting, never proves ownership retirement.
func (a *App) drainPrivate() {
	a.draining.Store(true)
	if remote, ok := a.brokerPlayback.(interface{ SetDraining(bool) }); ok {
		remote.SetDraining(true)
	}
	if a.playback != nil {
		a.playback.SetDraining(true)
	}
	a.stepDownForShutdown()
	deadline := time.NewTimer(time.Duration(a.deployment.settings.DrainSeconds) * time.Second)
	defer deadline.Stop()
	expired := false
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		op, done := context.WithTimeout(context.Background(), time.Second)
		pending, err := a.privatePending(op)
		done()
		if err == nil && pending == 0 {
			return
		}
		select {
		case <-deadline.C:
			if !expired {
				a.log.Warn("private runtime drain deadline reached; keeping membership and cleanup alive with unresolved ownership")
				expired = true
			}
		case <-tick.C:
		}
	}
}
