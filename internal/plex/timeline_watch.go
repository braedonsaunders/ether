package plex

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	watchstate "github.com/braedonsaunders/ether/internal/state"
)

type plexTimelineWriter interface {
	PlexTimeline(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string, string, *int) (watchstate.PlexTimelineResult, error)
}

func (s *Server) timelineWatch(ctx context.Context, pr *Request, key core.RatingKey, offset, duration time.Duration, status string, r *http.Request) (context.Context, *watchstate.PlexTimelineResult, error) {
	if suppressTimelineWatch(ctx) {
		return watchstate.WithPlexTimelineStored(ctx), nil, nil
	}
	writer, ok := s.d.State.(plexTimelineWriter)
	if !ok {
		return ctx, nil, nil
	}
	var offline *int
	if r.URL.Query().Get("offline") == "1" {
		n, err := strconv.Atoi(r.URL.Query().Get("viewCount"))
		if err != nil || n < 0 {
			n = -1
		}
		offline = &n
	}
	result, err := writer.PlexTimeline(ctx, pr.User.ID, key, offset, duration, status, r.URL.Query().Get("playQueueItemID"), offline)
	if err != nil {
		return ctx, nil, err
	}
	if result.Changed && (result.PlaybackState == "complete" || result.ViewOffset > time.Minute) {
		if parents, ok := s.d.State.(interface {
			PlexTouchParents(context.Context, int64, core.RatingKey, time.Time, bool) error
		}); ok {
			_ = parents.PlexTouchParents(ctx, pr.User.ID, key, s.now(), result.Completed)
		}
	}
	if result.Changed {
		if restore, ok := s.d.State.(interface {
			RestoreContinue(context.Context, int64, core.RatingKey) error
		}); ok {
			_ = restore.RestoreContinue(ctx, pr.User.ID, key)
		}
	}
	if result.Changed && s.d.Discovery != nil && s.d.Discovery.Personal != nil {
		_ = s.d.Discovery.Personal.RestoreContinue(ctx, pr.User.ID, key)
	}
	return watchstate.WithPlexTimelineStored(ctx), &result, nil
}
func timelineWatchAttrs(mc *model.MediaContainer, result *watchstate.PlexTimelineResult) {
	if result == nil {
		return
	}
	offset := int64(-1)
	if result.ViewOffset > 0 {
		offset = result.ViewOffset.Milliseconds()
	}
	mc.Extra = append(mc.Extra, model.A("playbackState", result.PlaybackState), model.A("viewOffset", offset), model.A("viewCount", result.ViewCount), model.A("skipCount", 0))
}
