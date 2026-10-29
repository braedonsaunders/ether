package frontend

import "context"

// VirtualPaths resolves a viewer-authorized item to the same relative path
// used by the WebDAV tree, including stable collision disambiguation.
type VirtualPaths interface {
	Path(context.Context, *Viewer, Device, RatingKey) (string, error)
}
