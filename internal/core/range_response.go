package core

import (
	"context"
	"net/http"
	"time"
)

// RangeRepresentation lets an adapter render byte-response headers and ranges
// while the shared source retains ownership of reads and cancellation.
type RangeRepresentation struct {
	Size              int64
	ETag, ContentType string
	ModTime           time.Time
	ServeBody         func(status int, start, length int64) error
}
type RangeResponse struct {
	// StrictRanges prevents an original-byte client from receiving a full file
	// after a malformed range or a failed representation precondition.
	StrictRanges bool
	MIME         func(string) string
	Serve        func(http.ResponseWriter, *http.Request, RangeRepresentation) error
}
type rangeResponseKey struct{}

func WithRangeResponse(ctx context.Context, policy RangeResponse) context.Context {
	return context.WithValue(ctx, rangeResponseKey{}, policy)
}
func RangeResponseFrom(ctx context.Context) (RangeResponse, bool) {
	p, ok := ctx.Value(rangeResponseKey{}).(RangeResponse)
	return p, ok
}
