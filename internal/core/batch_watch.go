package core

import "context"

// BatchWatchState updates a collection of watched states atomically.
type BatchWatchState interface {
	ScrobbleMany(context.Context, int64, []RatingKey) error
	UnscrobbleMany(context.Context, int64, []RatingKey) error
}
