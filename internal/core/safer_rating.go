package core

import "context"

type plexSaferRatingKey struct{}

// WithPlexSaferRating marks a request so catalogue SQL also applies cached
// Plex content-rating levels. Other frontends leave the context unset.
func WithPlexSaferRating(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, plexSaferRatingKey{}, true)
}

// PlexSaferRating reports that catalogue queries on ctx should hide titles
// whose cached Plex rating exceeds the viewer's limit.
func PlexSaferRating(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(plexSaferRatingKey{}).(bool)
	return v
}
