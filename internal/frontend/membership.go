package frontend

import (
	"context"
	"time"
)

// Membership reports an authorized title's presence and recorded addition
// time. AddedAt is when this viewer stored the membership. It is not the
// title's date added, and it is nil when no genuine stored time is known.
type Membership struct {
	Present bool
	AddedAt *time.Time
}

// MembershipReader reads one policy-authorized title at a time. An absent
// membership is distinct from an inaccessible title or inactive viewer.
type MembershipReader interface {
	FavoriteItem(context.Context, *Viewer, Device, RatingKey) (Membership, error)
	WatchlistItem(context.Context, *Viewer, Device, RatingKey) (Membership, error)
}
