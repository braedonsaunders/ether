package core

import "context"

type providerLeaseCancelKey struct{}

// WithProviderLeaseCancel lets a provider limiter stop the HTTP request if a
// distributed connection lease is lost. Callers still release on body close.
func WithProviderLeaseCancel(ctx context.Context, cancel context.CancelFunc) context.Context {
	return context.WithValue(ctx, providerLeaseCancelKey{}, cancel)
}

func ProviderLeaseCancel(ctx context.Context) context.CancelFunc {
	cancel, _ := ctx.Value(providerLeaseCancelKey{}).(context.CancelFunc)
	return cancel
}
