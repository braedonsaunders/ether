package sessions

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// SourceHistoryWriter stores facts from accepted samples, ordered first by
// session epoch and then by the sample number assigned before persistence.
type SourceHistoryWriter interface {
	BeginSourceSession(context.Context) (int64, error)
	RecordLastSource(context.Context, int64, core.RatingKey, int64, int64, core.PlaybackSourceObservation) error
}

type sourceHistorySample struct {
	viewer, epoch, sequence int64
	item                    core.RatingKey
	source                  core.PlaybackSourceObservation
}

// recordSourceHistory runs under the session owner gate. Source capture and
// order assignment precede queueing; no playback lock covers storage I/O.
func (b *Broker) recordSourceHistory(ctx context.Context, s *brokerSession) {
	snapshotter, ok := b.deps.Playback.(core.PlaybackSourceSnapshot)
	if !ok || b.historyQueue == nil || s.historyEpoch <= 0 {
		return
	}
	source, err := snapshotter.CurrentSource(ctx, s.playID)
	if err != nil {
		return
	}
	b.mu.Lock()
	current := b.sessions[s.public.ID]
	if current == nil || current.playID != s.playID || current.historyEpoch != s.historyEpoch {
		b.mu.Unlock()
		return
	}
	current.historySequence++
	sequence := current.historySequence
	b.mu.Unlock()
	sample := sourceHistorySample{viewer: s.public.ViewerID, item: s.public.Item, epoch: s.historyEpoch, sequence: sequence, source: source}
	select {
	case b.historyQueue <- sample:
	default:
		if b.deps.OnEventError != nil {
			b.deps.OnEventError(errors.New("sessions: source history queue full"))
		}
	}
}

func (b *Broker) runSourceHistory(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case sample := <-b.historyQueue:
			if ctx.Err() != nil {
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
			err := b.deps.SourceHistory.RecordLastSource(writeCtx, sample.viewer, sample.item, sample.epoch, sample.sequence, sample.source)
			cancel()
			if err != nil && b.deps.OnEventError != nil {
				b.deps.OnEventError(err)
			}
		}
	}
}
