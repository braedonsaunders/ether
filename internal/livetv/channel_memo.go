package livetv

import (
	"context"
	"sync"
)

type channelMemoKey struct{}

type channelMemo struct {
	mu      sync.Mutex
	decided map[string]bool
}

// WithChannelMemo shares one channel decision per id for the rest of this
// request. Row admission checks the same channel many times.
func WithChannelMemo(ctx context.Context) context.Context {
	if _, ok := channelMemoFrom(ctx); ok {
		return ctx
	}
	return context.WithValue(ctx, channelMemoKey{}, &channelMemo{})
}

func channelMemoFrom(ctx context.Context) (*channelMemo, bool) {
	memo, ok := ctx.Value(channelMemoKey{}).(*channelMemo)
	return memo, ok && memo != nil
}

func (m *channelMemo) get(id string) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	decided, ok := m.decided[id]
	return decided, ok
}

func rememberChannel(ctx context.Context, id string, allowed bool) {
	memo, ok := channelMemoFrom(ctx)
	if !ok {
		return
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if memo.decided == nil {
		memo.decided = map[string]bool{}
	}
	memo.decided[id] = allowed
}
