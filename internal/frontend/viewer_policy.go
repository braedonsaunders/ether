package frontend

import "context"

// ViewerPolicy mutates household policy with host-side authorization.
type ViewerPolicy interface {
	SetMaxRating(context.Context, *Viewer, string) error
}
