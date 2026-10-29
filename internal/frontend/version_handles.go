package frontend

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// VersionScope binds a version handle to an authenticated viewer, surface,
// device and host-built client capabilities. It grants no playback by itself.
type VersionScope struct {
	Viewer   *core.Viewer
	Device   core.Device
	Protocol Protocol
	Caps     core.ClientCaps
}

// VersionTarget is the authorized parent associated with an opaque handle.
// A caller must still open playback and revalidate the selected source.
type VersionTarget struct{ Item core.RatingKey }

type VersionHandles interface {
	Issue(context.Context, VersionScope, core.RatingKey, string) (string, error)
	Resolve(context.Context, VersionScope, string) (VersionTarget, error)
}
