package frontend

import (
	"context"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

type requestMemoKey struct{}

// AiredMemo is one show's capped local episode page. Season rows on the same
// request share it instead of reading the show again for every season.
type AiredMemo struct {
	Items []core.Title
	Total int
	Err   error
}

// RequestMemo keeps one viewer snapshot and one visibility decision per title
// for the rest of a single request.
type RequestMemo struct {
	Mu      sync.Mutex
	Viewer  *core.User
	Visible map[core.RatingKey]bool
	Aired   map[core.RatingKey]AiredMemo
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
