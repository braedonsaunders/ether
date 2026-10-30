//go:build plexparity && conformlab

package sessions

// ConformLabLifecycleCounters returns only numeric in-memory ownership counts. It is
// excluded from ordinary builds and never returns identities or source paths.
func (b *Broker) ConformLabLifecycleCounters(viewer int64) map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int{"plans": len(b.plans), "pending": b.pendingPlans, "viewer_pending": b.pendingByViewer[viewer], "max_viewer_plans": maxViewerPlans, "max_broker_plans": maxBrokerPlans, "source_leases": len(b.sourceLeases), "broker_sessions": len(b.sessions), "detail_cache_entries": len(b.detailPlans), "plan_ttl_seconds": int(planLifetime.Seconds())}
	// Completed cache entries are not running flights or retained source leases.
	for _, entry := range b.detailPlans {
		select {
		case <-entry.done:
			out["detail_completed"]++
		default:
			out["detail_inflight"]++
		}
	}

	for _, p := range b.plans {
		if p.request.Viewer != nil && p.request.Viewer.ID == viewer {
			out["viewer_plans"]++
			if b.deps.Now().Sub(p.created) > planLifetime {
				out["viewer_expired_plans"]++
			}
		}
	}
	for _, s := range b.sourceLeases {
		out["source_plan_refs"] += s.plans
		out["source_session_refs"] += s.sessions
		if s.owned {
			out["owned_source_leases"]++
		}
	}
	return out
}
