package sessions

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ProgressWriter persists the timeline after the caller has checked ownership.
type ProgressWriter interface {
	Progress(context.Context, int64, core.RatingKey, time.Duration, time.Duration, string) error
}

// RecordProgress preserves the existing treatment of extras, which do not
// enter the viewer's library watch history.
func RecordProgress(ctx context.Context, writer ProgressWriter, viewer int64, key core.RatingKey, position, duration time.Duration, state string) error {
	if writer == nil || key.Kind() == core.KindExtra {
		return nil
	}
	return writer.Progress(ctx, viewer, key, position, duration, state)
}
