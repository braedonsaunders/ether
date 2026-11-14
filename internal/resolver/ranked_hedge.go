package resolver

import (
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// The owner-preferred source gets a head start, but an already validated
// equivalent must not sit behind a provider's entire eight-second deadline.
const rankedSourceGrace = 2 * time.Second

// A health check that downloads an NZB (or a torrent file) spends one of the
// indexer's daily grabs. A second such check starts only as a hedge: when
// the one in flight fails, or has run longer than a healthy check usually
// takes. A healthy title then costs one grab, and a slow one still gets its
// backup well inside the grace a backup must wait anyway.
const (
	defaultHealthHedgeDelay = 1500 * time.Millisecond
	minHealthHedgeDelay     = 750 * time.Millisecond
)

// spendsGrab reports whether checking rel downloads from its indexer: a
// Usenet NZB, or a torrent known only by its download link. Torrents with
// an info hash or magnet are checked at the debrid provider alone.
func spendsGrab(rel core.Release) bool {
	switch rel.Protocol {
	case core.ProtocolTorrent:
		return rel.InfoHash == "" && rel.MagnetURL == ""
	case core.ProtocolHTTP:
		return false
	}
	return true
}

// healthHedgeDelay is how long a grab-spending check runs alone before a
// hedge starts: a quarter more than this protocol's usual startup once known,
// within [minHealthHedgeDelay, rankedSourceGrace].
func (s *Service) healthHedgeDelay(rel core.Release) time.Duration {
	if avg, ok := s.startupOf(protocolOf(rel)); ok {
		return min(rankedSourceGrace, max(minHealthHedgeDelay, avg+avg/4))
	}
	return defaultHealthHedgeDelay
}

func equivalentHedge(lead, backup candidate, margin float64) bool {
	return samePreferenceTier(lead, backup) && releasepolicy.Equivalent(lead.policy, backup.policy) && pluginKeysEqual(lead, backup) &&
		!lead.verified && backup.bandwidth >= lead.bandwidth &&
		sameTier(backup, lead) &&
		backup.rel.Info.Edition == lead.rel.Info.Edition &&
		backup.rel.Score >= lead.rel.Score-margin
}
