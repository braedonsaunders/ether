package resolver

import (
	"time"

	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// The owner-preferred source gets a head start, but an already validated
// equivalent must not sit behind a provider's entire eight-second deadline.
const rankedSourceGrace = 2 * time.Second

func equivalentHedge(lead, backup candidate, margin float64) bool {
	return releasepolicy.Equivalent(lead.policy, backup.policy) &&
		!lead.verified && backup.bandwidth >= lead.bandwidth &&
		sameTier(backup, lead) &&
		backup.rel.Info.Edition == lead.rel.Info.Edition &&
		backup.rel.Score >= lead.rel.Score-margin
}

// waitRankedValidation preserves rank order during grace. A healthy equivalent
// may win afterwards; an unhealthy backup never cuts the preferred check short.
// A consumed backup is restored for the ordinary ranked loop if preferred wins.
func waitRankedValidation(preferred, backup chan validation, grace time.Duration) validation {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	var ready *validation
	defer func() {
		if ready != nil {
			backup <- *ready
		}
	}()
	backupEvents := (<-chan validation)(backup)
	graceExpired := false
	for {
		// If both become ready together, keep the owner's preferred source.
		select {
		case v := <-preferred:
			return v
		default:
		}
		if graceExpired && ready != nil && ready.healthy && ready.err == nil {
			return *ready
		}
		select {
		case v := <-preferred:
			return v
		case v := <-backupEvents:
			ready = &v
			backupEvents = nil
		case <-timer.C:
			graceExpired = true
		}
	}
}
