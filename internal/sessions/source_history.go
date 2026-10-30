package sessions

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// SourceHistoryWriter persists a source snapshot for an accepted session
// sample. Epochs are shared and order sessions across cluster nodes.
type SourceHistoryWriter interface {
	BeginSourceSession(context.Context) (int64, error)
	RecordLastSource(context.Context, int64, core.RatingKey, int64, core.PlaybackSourceObservation) error
}

func (b *Broker) recordSourceHistory(ctx context.Context, s *brokerSession) {
	guard, ok := b.deps.Playback.(core.GuardedPlaybackSource)
	if !ok || b.deps.SourceHistory == nil || s.historyEpoch <= 0 {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	err := guard.WithCurrentSource(bounded, s.playID, func(ctx context.Context, source core.PlaybackSourceObservation) error {
		return b.deps.SourceHistory.RecordLastSource(ctx, s.public.ViewerID, s.public.Item, s.historyEpoch, source)
	})
	if err != nil && b.deps.OnEventError != nil {
		b.deps.OnEventError(err)
	}
}
