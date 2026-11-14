package frontend

import (
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ErrNotFound identifies an absent or inaccessible frontend resource.
// It shares identity with the domain error for errors.Is checks.
var ErrNotFound = core.ErrNotFound

// ErrUnauthorized identifies a viewer who cannot see or change the resource.
// It shares identity with the domain error for errors.Is checks.
var ErrUnauthorized = core.ErrUnauthorized

// ErrNotAvailable means the host has no service for this browse feature.
// It shares identity with the domain error for errors.Is checks.
var ErrNotAvailable = core.ErrNotAvailable

// ErrRowViewStale asks the client to refresh its visit-scoped row views.
var ErrRowViewStale = errors.New("frontend: row view snapshot expired or changed")

// ErrStreamLimit identifies exhausted playback admission.
var ErrStreamLimit = core.ErrStreamLimit

// ErrDownloadBudget identifies an exceeded temporary download disk budget.
// It shares identity with the domain error for errors.Is checks.
var ErrDownloadBudget = core.ErrDownloadBudget

// ErrDownloadNeedsTranscode identifies a download source that cannot be
// copied to MP4. It shares identity with the domain error for errors.Is.
var ErrDownloadNeedsTranscode = core.ErrDownloadNeedsTranscode

// ErrInteractionReceipt is an invalid, stale or out-of-scope interaction handle.
var ErrInteractionReceipt = errors.New("experience: invalid interaction receipt")

// IdentityRateLimitError preserves the identity denial and its fixed-window
// retry delay without exposing the peer, username or bucket identifiers.
type IdentityRateLimitError struct{ RetryAfter time.Duration }

func (*IdentityRateLimitError) Error() string { return ErrRateLimited.Error() }
func (*IdentityRateLimitError) Unwrap() error { return ErrRateLimited }
