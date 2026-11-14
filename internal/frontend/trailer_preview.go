package frontend

import (
	"context"
	"os"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// TrailerPreviewAvailability reports current owner/resolver/cache readiness without work.
type TrailerPreviewAvailability interface{ TrailerPreviewAvailable() bool }

// TrailerPreviews is optional. Call only after viewer focus/open, never while
// projecting homes or rows. Client capabilities must be authenticated/bounded
// by the protocol adapter before filling Device and Caps.
type TrailerPreviews interface {
	TrailerPreview(context.Context, TrailerPreviewRequest) (TrailerPreviewResult, error)
}
type TrailerPreviewRequest struct {
	Viewer         *core.Viewer
	Device         core.Device
	Protocol       Protocol
	Item           core.RatingKey
	IdempotencyKey string
	Language       string
	Caps           core.ClientCaps
}
type TrailerPreviewResult struct {
	State             string               `json:"state"`
	Grant             *TrailerPreviewGrant `json:"grant,omitempty"`
	RetryAfterSeconds int                  `json:"retryAfterSeconds,omitempty"`
	Reason            string               `json:"reason,omitempty"`
}
type TrailerPreviewGrant struct {
	SessionID        string    `json:"sessionId"`
	URL              string    `json:"url"`
	MIMEType         string    `json:"mimeType"`
	ExpiresAt        time.Time `json:"expiresAt"`
	Purpose          string    `json:"purpose"`
	SourceGeneration int64     `json:"sourceGeneration"`
	SeekGeneration   int64     `json:"seekGeneration"`
}

// CachedExtraStreams serves the same durable bytes to authenticated local
// trailer endpoints. The caller must authorize the parent before opening.
type CachedExtraStreams interface {
	OpenCachedExtra(context.Context, core.RatingKey) (*os.File, string, error)
}
