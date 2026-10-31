package frontend

import (
	"context"
	"time"
)

type synchronousArtworkMetadataKey struct{}

// WithSynchronousArtworkMetadata lets a caller wait briefly for hashing that
// is already in flight. Stored rows are joined without this budget. The page
// cache clears it before filling a page, so a browse never holds the cache
// open for a hash.
func WithSynchronousArtworkMetadata(ctx context.Context, budget time.Duration) context.Context {
	if ctx == nil || budget <= 0 {
		return ctx
	}
	return context.WithValue(ctx, synchronousArtworkMetadataKey{}, time.Now().Add(budget))
}

// WithoutSynchronousArtworkMetadata drops an in-flight artwork wait so a
// caller, such as the browse page cache, returns as soon as stored rows are joined.
func WithoutSynchronousArtworkMetadata(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, synchronousArtworkMetadataKey{}, nil)
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
