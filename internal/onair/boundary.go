package onair

import (
	"context"
	"time"
)

type boundaryDeadlineKey struct{}

// WithBoundaryDeadline identifies an imminent master without changing the
// job's cancellation deadline or its content-addressed cache key.
func WithBoundaryDeadline(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, boundaryDeadlineKey{}, at)
}

func BoundaryDeadline(ctx context.Context) (time.Time, bool) {
	at, ok := ctx.Value(boundaryDeadlineKey{}).(time.Time)
	return at, ok && !at.IsZero()
}
