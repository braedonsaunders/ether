package frontend

import "context"

// LastPlayedSourceReader returns source facts only after current item policy
// authorization. Nil means no trustworthy source facts are available.
type LastPlayedSourceReader interface {
	LastPlayedSource(context.Context, *Viewer, Device, RatingKey) (*PlaybackSourceHistory, error)
}
