package core

import "context"

type streamKeyKey struct{}

// WithStreamKey tags ctx with the playback stream its reads serve, so the
// Usenet fetcher can tell a stalled read from ordinary variance between
// concurrent viewers. key must be comparable (a pointer, typically); nil
// leaves ctx unchanged.
func WithStreamKey(ctx context.Context, key any) context.Context {
	if key == nil {
		return ctx
	}
	return context.WithValue(ctx, streamKeyKey{}, key)
}

// StreamKey returns the key set by WithStreamKey, or nil.
func StreamKey(ctx context.Context) any { return ctx.Value(streamKeyKey{}) }
