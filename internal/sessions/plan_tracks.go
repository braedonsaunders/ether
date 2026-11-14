package sessions

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// pinRequestedTracks binds a plan to the exact source revision when the
// client asked for a specific audio or subtitle stream, and pins that
// revision so the source cannot silently fail over under the client's track
// choice. It is shared by the catalogue plan and the plugin kind plan so both
// honour explicit stream-index selection with identical ETag and pinning
// semantics (spec §15.1).
//
// It returns the current session snapshot (which may have been re-read under
// the pin) and whether a pin was taken; the caller must release it with
// releasePlanSource on a plan that is not stored. On error it returns the
// original ps unchanged (never nil) so the caller's deferred release stays
// valid.
func (b *Broker) pinRequestedTracks(ctx context.Context, req *frontend.PlanRequest, ps *core.PlaySession) (*core.PlaySession, bool, error) {
	if req.Intent.AudioOrdinal != nil || req.Intent.SubtitleStreamIndex != nil || (req.Protocol == "vela" && req.Intent.OriginalPlayback.Valid()) {
		if ps.Info.Estimated {
			return ps, false, frontend.ErrTrackSourceUnavailable
		}
		if req.ExpectedSourceETag == "" {
			req.ExpectedSourceETag = sourceRevision(ps)
			if req.ExpectedSourceETag == "" {
				return ps, false, frontend.ErrTrackSourceUnavailable
			}
		}
	}
	if req.ExpectedSourceETag == "" {
		return ps, false, nil
	}
	pins, ok := b.deps.Playback.(core.PlaybackRevisionPin)
	if !ok {
		if req.Intent.AudioOrdinal != nil || req.Intent.SubtitleStreamIndex != nil {
			return ps, false, frontend.ErrTrackSourceUnavailable
		}
		return ps, false, frontend.ErrSourceGone
	}
	if err := pins.PinSourceRevision(ctx, ps.ID, req.ExpectedSourceETag); err != nil {
		if ctx.Err() != nil {
			return ps, false, ctx.Err()
		}
		if errors.Is(err, core.ErrSourceChanged) {
			return ps, false, frontend.ErrSourceGone
		}
		return ps, false, err
	}
	current, ok := b.deps.Playback.Session(ps.ID)
	if !ok || current == nil || sourceRevision(current) != req.ExpectedSourceETag {
		return ps, true, frontend.ErrSourceGone
	}
	return current, true, nil
}
