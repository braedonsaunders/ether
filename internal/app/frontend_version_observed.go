package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type candidateObservationLedger interface {
	ObserveBatch(context.Context, core.RatingKey, []string, time.Time) (map[string]time.Time, error)
}

// observeVersionOffers attaches only persisted timestamps for actual candidate
// release IDs. Failure leaves times unknown and does not suppress browsing.
func observeVersionOffers(ctx context.Context, ledger candidateObservationLedger, parent core.RatingKey, offers []core.ReleaseVariant, versions []frontend.Version, observed time.Time) error {
	stamps, err := experience.ObserveVersionOffers(ctx, ledger, parent, experience.VersionOffersFrom(nil, offers), len(versions), observed)
	if err != nil {
		return err
	}
	for i, stamp := range stamps {
		if stamp == nil || i >= len(versions) {
			continue
		}
		versions[i].FirstObservedAt = stamp
	}
	return nil
}
