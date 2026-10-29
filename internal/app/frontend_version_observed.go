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
	sources := make([]string, 0, len(offers))
	byIndex := make(map[int]string)
	for i, offer := range offers {
		if i >= len(versions) {
			break
		}
		source, ok := experience.CandidateObservationSource(offer.Release.ID)
		if !ok {
			continue
		}
		sources = append(sources, source)
		byIndex[i] = source
	}
	if len(sources) == 0 {
		return nil
	}
	times, err := ledger.ObserveBatch(ctx, parent, sources, observed)
	if err != nil {
		return err
	}
	for i, source := range byIndex {
		if first, ok := times[source]; ok {
			value := first
			versions[i].FirstObservedAt = &value
		}
	}
	return nil
}
