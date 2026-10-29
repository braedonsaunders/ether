package frontendtest

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// ImageGrants provides optional hooks for scoped individual and batch grants.
type ImageGrants struct {
	IssueFunc      func(context.Context, frontend.ImageGrantRequest) (string, error)
	IssueBatchFunc func(context.Context, frontend.ImageBatchRequest) ([]frontend.ImageBatchItem, error)
	RevokeFunc     func(context.Context, string, string) error
	ServeFunc      func(http.ResponseWriter, *http.Request, string)
}

func (f ImageGrants) Issue(ctx context.Context, req frontend.ImageGrantRequest) (string, error) {
	if f.IssueFunc != nil {
		return f.IssueFunc(ctx, req)
	}
	return "", core.ErrNotAvailable
}
func (f ImageGrants) IssueBatch(ctx context.Context, req frontend.ImageBatchRequest) ([]frontend.ImageBatchItem, error) {
	if f.IssueBatchFunc != nil {
		return f.IssueBatchFunc(ctx, req)
	}
	return nil, core.ErrNotAvailable
}
func (f ImageGrants) Revoke(ctx context.Context, credential, secret string) error {
	if f.RevokeFunc != nil {
		return f.RevokeFunc(ctx, credential, secret)
	}
	return core.ErrNotAvailable
}
func (f ImageGrants) Serve(w http.ResponseWriter, r *http.Request, secret string) {
	if f.ServeFunc != nil {
		f.ServeFunc(w, r, secret)
		return
	}
	http.NotFound(w, r)
}

var _ frontend.ImageGrants = ImageGrants{}
var _ frontend.ImageGrantBatch = ImageGrants{}
