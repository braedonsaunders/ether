package core

import "context"

// SearchCacheRefiller requests best-effort, non-blocking candidate recovery for
// an admitted detail warm-up reusing a remembered source. It searches only;
// it must not resolve or replace a playback source. The resolver owns the
// work independently of request/warm-up cancellation and joins it on Close.
// A nil signal means skipped; otherwise the signal closes when the existing
// or newly admitted refill finishes (not necessarily successfully).
type SearchCacheRefiller interface {
	RefillSearchCache(*Title) <-chan struct{}
}

// ReleaseBlockChecker lets stored source consumers honor durable owner blocks.
// Errors must prevent reuse, rather than silently bypassing the blocklist.
type ReleaseBlockChecker interface {
	ReleaseBlocked(context.Context, string) (bool, error)
}
