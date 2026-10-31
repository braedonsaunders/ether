package frontend

import (
	"context"
	"time"
)

type synchronousArtworkMetadataKey struct{}

// WithSynchronousArtworkMetadata hashes artwork before it is granted, until
// the budget expires. Callers that publish blurhashes on the first response
// use it. Other surfaces leave hashing on the background worker.
func WithSynchronousArtworkMetadata(ctx context.Context, budget time.Duration) context.Context {
	if ctx == nil || budget <= 0 {
		return ctx
	}
	return context.WithValue(ctx, synchronousArtworkMetadataKey{}, time.Now().Add(budget))
}

// SynchronousArtworkDeadline is the time after which artwork hashing returns
// to the background worker. The second result is false when no budget was set.
func SynchronousArtworkDeadline(ctx context.Context) (time.Time, bool) {
	if ctx == nil {
		return time.Time{}, false
	}
	deadline, ok := ctx.Value(synchronousArtworkMetadataKey{}).(time.Time)
	return deadline, ok
}
