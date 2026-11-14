package core

import "context"

// VibeSearcher is optionally implemented by the collections engine: it
// returns a cached AI-generated collection for the viewer, alongside its
// top items. A cache miss schedules bounded work and returns ok=false;
// ordinary title search remains available without waiting for the model.
type VibeSearcher interface {
	VibeCollection(ctx context.Context, user *User, query string) (c *Collection, items []Title, ok bool, err error)
	// PromoteEphemeral saves an ephemeral collection as a persisted
	// personal smart collection for its owner. ok is false when key is
	// not a live ephemeral collection owned by user.
	PromoteEphemeral(ctx context.Context, user *User, key RatingKey) (c *Collection, ok bool, err error)
}
