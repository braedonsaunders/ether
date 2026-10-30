package frontend

import (
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// ErrNotFound identifies an absent or inaccessible frontend resource.
// It shares identity with the domain error for errors.Is checks.
var ErrNotFound = core.ErrNotFound

// ErrNotAvailable means the host has no service for this browse feature.
// It shares identity with the domain error for errors.Is checks.
var ErrNotAvailable = core.ErrNotAvailable

// ErrRowViewStale asks the client to refresh its visit-scoped row views.
var ErrRowViewStale = errors.New("frontend: row view snapshot expired or changed")
