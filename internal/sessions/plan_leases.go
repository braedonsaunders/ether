package sessions

import (
	"context"
	"fmt"
	"time"
)

const (
	planLifetime   = 2 * time.Minute
	maxBrokerPlans = 1024
	maxViewerPlans = 32
)

type sourceLease struct {
	plans, sessions int
	owned           bool
}

// reservePlan bounds queued/in-flight plans globally and per viewer. Expired
// plans remain counted until the sweeper releases their source lease.
func (b *Broker) reservePlan(viewer int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.plans)+b.pendingPlans >= maxBrokerPlans {
		return false
	}
	count := b.pendingByViewer[viewer]
	for _, p := range b.plans {
		if p.request.Viewer != nil && p.request.Viewer.ID == viewer {
			count++
		}
	}
	if count >= maxViewerPlans {
		return false
	}
	b.pendingPlans++
	b.pendingByViewer[viewer]++
	if b.planSweepStop == nil {
		b.planSweepStop = make(chan struct{})
		b.planSweepDone = make(chan struct{})
		go b.runPlanSweeper(b.planSweepStop, b.planSweepDone, b.planSweepEvery)
	}
	return true
}
func (b *Broker) releasePlanReservation(viewer int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pendingPlans--
	b.pendingByViewer[viewer]--
	if b.pendingByViewer[viewer] == 0 {
		delete(b.pendingByViewer, viewer)
	}
}
func (b *Broker) acquirePlanSource(id string, owned bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lease := b.sourceLeases[id]
	if lease == nil {
		lease = &sourceLease{owned: owned}
		b.sourceLeases[id] = lease
	}
	lease.owned = lease.owned || owned
	lease.plans++
}

// The caller holds this source's owner gate. Stop outside b.mu while the gate
// prevents another plan or session from claiming the source.
func (b *Broker) releasePlanSource(id string) {
	b.mu.Lock()
	lease := b.sourceLeases[id]
	stop := false
	if lease != nil {
		lease.plans--
		if lease.plans == 0 && lease.sessions == 0 {
			stop = lease.owned
			delete(b.sourceLeases, id)
		}
	}
	b.mu.Unlock()
	if stop {
		b.deps.Playback.Stop(id)
	}
}

// Caller holds b.mu and the owner gate.
func (b *Broker) transferPlanSourceLocked(id string) {
	if lease := b.sourceLeases[id]; lease != nil {
		lease.plans--
		lease.sessions++
		lease.owned = true
	}
}

// Caller holds b.mu and the owner gate. A nil lease preserves old tracker
// behavior for entries that were created outside the broker.
func (b *Broker) releaseSessionSourceLocked(id string) bool {
	lease := b.sourceLeases[id]
	if lease == nil {
		return true
	}
	lease.sessions--
	if lease.plans == 0 && lease.sessions == 0 {
		stop := lease.owned
		delete(b.sourceLeases, id)
		return stop
	}
	return false
}
func (b *Broker) runPlanSweeper(stop chan struct{}, done chan struct{}, interval time.Duration) {
	defer func() {
		b.mu.Lock()
		if b.planSweepStop == stop {
			b.planSweepStop = nil
			b.planSweepDone = nil
		}
		b.mu.Unlock()
		close(done)
	}()
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			b.expirePlans(stop)
			b.mu.Lock()
			idle := len(b.plans) == 0 && b.pendingPlans == 0
			if idle && b.planSweepStop == stop {
				b.planSweepStop = nil
				b.planSweepDone = nil
			}
			b.mu.Unlock()
			if idle {
				return
			}
		}
	}
}
func (b *Broker) expirePlans(stop <-chan struct{}) {
	b.mu.Lock()
	ids := make([]string, 0, len(b.plans))
	now := b.deps.Now()
	for id, p := range b.plans {
		if !p.opening && (b.closed || now.Sub(p.created) > planLifetime) {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		select {
		case <-stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		b.mu.Lock()
		closing := b.closed
		b.mu.Unlock()
		b.dropPlan(ctx, id, closing)
		cancel()
	}
}

// dropPlan serializes expiry/close with Plan and Open using the same owner
// gate. It checks the plan again after acquiring the gate.
func (b *Broker) dropPlan(ctx context.Context, id string, force bool) bool {
	b.mu.Lock()
	p, ok := b.plans[id]
	b.mu.Unlock()
	if !ok || p.request.Viewer == nil {
		return false
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", p.request.Viewer.ID, p.request.Caps.ClientID))
	if err != nil {
		return false
	}
	defer unlock()
	b.mu.Lock()
	current, ok := b.plans[id]
	if !ok || current.opening || (!force && b.deps.Now().Sub(current.created) <= planLifetime) {
		b.mu.Unlock()
		return false
	}
	delete(b.plans, id)
	b.mu.Unlock()
	if current.source != nil {
		b.releasePlanSource(current.source.ID)
	}
	return true
}
