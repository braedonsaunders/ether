package playback

import "github.com/braedonsaunders/ether/internal/core"

// ObserveClientThroughput receives accepted timeline evidence for an actual
// playback session. Its client identity comes from playback admission.
func (m *Manager) ObserveClientThroughput(id string, bps float64) {
	if observer, ok := m.deps.Resolver.(core.ClientDeliveryEstimator); ok {
		if s := m.lookup(id); s != nil {
			observer.ObserveClientThroughput(s.ident.userID, s.caps, bps)
		}
	}
}

// hasAuthoritativeAutoChoice reports whether an owner or viewer choice, not
// ranking, put releaseID in Auto: a strict pin, the preferred release itself,
// or a viewer's remembered choice. Such a source is not replaced to fit an
// estimate.
func (m *Manager) hasAuthoritativeAutoChoice(s *session, releaseID string) bool {
	if pin, ok := m.deps.Resolver.(interface {
		PinnedReleaseID(core.ResolveRequest) string
	}); ok &&
		pin.PinnedReleaseID(core.ResolveRequest{Title: s.title, Client: s.caps, User: &s.user}) != "" {
		return true
	}
	if id, _ := m.preferredRelease(s); id != "" && id == releaseID {
		return true
	}
	reader, ok := m.deps.Resolver.(viewerChoiceReader)
	return ok && reader.ActiveViewerChoiceRelease(core.ResolveRequest{Title: s.title, Client: s.caps, User: &s.user}) != ""
}

func (m *Manager) clientDeliveryBudget(s *session) int {
	if observer, ok := m.deps.Resolver.(core.ClientDeliveryEstimator); ok {
		return observer.ClientDeliveryBudgetKbps(s.ident.userID, s.caps)
	}
	return 0
}
