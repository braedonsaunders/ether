package frontend

import (
	"context"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

type requestMemoKey struct{}

// RequestMemo keeps one viewer snapshot and one visibility decision per title
// for the rest of a single request.
type RequestMemo struct {
	Mu      sync.Mutex
	Viewer  *core.User
	Visible map[core.RatingKey]bool
}

// WithRequestMemo installs the per-request viewer and visibility memo.
func WithRequestMemo(ctx context.Context) context.Context {
	if ctx == nil || ctx.Value(requestMemoKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, requestMemoKey{}, &RequestMemo{})
}

// RequestMemoFrom returns the memo installed by WithRequestMemo.
func RequestMemoFrom(ctx context.Context) *RequestMemo {
	if ctx == nil {
		return nil
	}
	memo, _ := ctx.Value(requestMemoKey{}).(*RequestMemo)
	return memo
}
