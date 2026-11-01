package frontend

import (
	"context"
	"net/http"
)

// VirtualPaths resolves a viewer-authorized item to the same relative path
// used by the WebDAV tree, including stable collision disambiguation.
type VirtualPaths interface {
	Path(context.Context, *Viewer, Device, RatingKey) (string, error)
	// Paths resolves at most 50 distinct keys (256 input entries), omitting
	// hidden or unsupported items and sharing viewer-scoped library scans.
	Paths(context.Context, *Viewer, Device, []RatingKey) (map[RatingKey]string, error)
	// Base returns the verified reachable WebDAV base URL, or an error when
	// WebDAV is disabled or unreachable for the requesting client.
	Base(context.Context, *http.Request) (string, error)
}
