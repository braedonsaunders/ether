package playback

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// A remembered release must not bypass an owner's explicit source choice.
// Resolve again when it belongs to the other source: the resolver still applies
// quality policy, pins and availability, and can fall back when needed.
func (m *Manager) resumeSourceAllowed(release core.Release) bool {
	switch strings.ToLower(strings.TrimSpace(m.config().Resolver.SourcePreference)) {
	case "debrid":
		return release.Protocol == core.ProtocolTorrent || release.Protocol == core.ProtocolHTTP
	case "usenet":
		return release.Protocol != core.ProtocolTorrent && release.Protocol != core.ProtocolHTTP
	default:
		return true
	}
}
