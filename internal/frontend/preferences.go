package frontend

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

var ErrPreferencesLimit = errors.New("frontend preferences limit exceeded")
var ErrInvalidPreferences = errors.New("invalid frontend preferences")

// ErrPreferencesNotFound identifies a missing record without exposing storage.
var ErrPreferencesNotFound = core.ErrNotFound

// Preferences stores presentation state for the bound surface. Viewer must be
// obtained from the current authenticated session. These values are not policy.
type Preferences interface {
	Get(context.Context, *core.Viewer, string, string) (json.RawMessage, error)
	Put(context.Context, *core.Viewer, string, string, json.RawMessage) error
	Delete(context.Context, *core.Viewer, string, string) error
}
