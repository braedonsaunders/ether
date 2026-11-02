package core

import (
	"context"
	"sync/atomic"
)

type speculativeKey struct{}

// WithSpeculative marks ctx as speculative work: a warm-up for a title
// nobody has pressed Play on yet. The returned promote clears the mark
// when a Play takes the work over, so from then on it runs as a real play.
func WithSpeculative(ctx context.Context) (context.Context, func()) {
	flag := new(atomic.Bool)
	flag.Store(true)
	return context.WithValue(ctx, speculativeKey{}, flag), func() { flag.Store(false) }
}

// IsSpeculative reports whether ctx is still speculative work.
func IsSpeculative(ctx context.Context) bool {
	flag, _ := ctx.Value(speculativeKey{}).(*atomic.Bool)
	return flag != nil && flag.Load()
}
