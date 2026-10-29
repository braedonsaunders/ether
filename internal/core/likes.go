package core

import "context"

// LikesState atomically applies a viewer's tri-state like to the shared rating.
// Nil clears the rating; true and false preserve existing compatible ratings.
type LikesState interface {
	SetLikes(context.Context, int64, RatingKey, *bool) error
}
