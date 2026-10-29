package sessions

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// Estimator is the non-resolving playback surface for detail pages.
type Estimator interface {
	Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo
	SessionFor(int64, string, core.RatingKey) (*core.PlaySession, bool)
}

// Estimate returns an existing source's media information or a virtual
// estimate. It never prepares or resolves a release.
func Estimate(ctx context.Context, playback Estimator, viewer *core.Viewer, clientID string, title *core.Title, caps func() core.ClientCaps) (core.MediaInfo, *core.PlaySession) {
	if playback == nil {
		return core.MediaInfo{Estimated: true, Duration: title.Runtime}, nil
	}
	if viewer != nil {
		if ps, ok := playback.SessionFor(viewer.ID, clientID, title.Key); ok && ps != nil {
			return ps.Info, ps
		}
	}
	return playback.Estimate(ctx, title, caps()), nil
}
