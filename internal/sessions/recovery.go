package sessions

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type inputFailover interface {
	FailoverInput(context.Context, string, string, time.Duration) error
}

// Recover maps an encoder identifier to the source that owns it. Playback
// verifies the input generation before it attempts failover.
func Recover[R any](ctx context.Context, table *BindingTable[R], playback any, id, inputURL string, at time.Duration) error {
	entry, ok := table.Get(id)
	if !ok || entry.PlayID == "" {
		return nil
	}
	p, ok := playback.(inputFailover)
	if !ok {
		return core.ErrNotAvailable
	}
	return p.FailoverInput(ctx, entry.PlayID, inputURL, at)
}

// TranscodeFor finds the running encoder fed by a playback source.
func TranscodeFor[R any](table *BindingTable[R], transcoder core.Transcoder, playID string) (core.HLSSession, bool) {
	if transcoder == nil {
		return nil, false
	}
	for _, id := range table.ByPlayID(playID) {
		if hs, ok := transcoder.Session(id); ok && hs != nil {
			return hs, true
		}
	}
	return nil, false
}
