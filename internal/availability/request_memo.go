package availability

import (
	"context"
	"sync"
)

type discoveryMemoKey struct{}

type discoveryMemo struct {
	mu    sync.Mutex
	ready bool
	snap  discoverySnapshot
}

// WithDiscoveryMemo shares one availability snapshot for the rest of this
// request. Home authorizes the same titles many times, and each check was
// cloning config and feed settings.
func WithDiscoveryMemo(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := discoveryMemoFrom(ctx); ok {
		return ctx
	}
	return context.WithValue(ctx, discoveryMemoKey{}, &discoveryMemo{})
}

func discoveryMemoFrom(ctx context.Context) (*discoveryMemo, bool) {
	if ctx == nil {
		return nil, false
	}
	memo, ok := ctx.Value(discoveryMemoKey{}).(*discoveryMemo)
	return memo, ok && memo != nil
}

func (s *Service) snapshotFor(ctx context.Context) discoverySnapshot {
	memo, ok := discoveryMemoFrom(ctx)
	if !ok {
		return s.discoverySnapshot()
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if !memo.ready {
		memo.snap = s.discoverySnapshot()
		memo.ready = true
	}
	return memo.snap
}
