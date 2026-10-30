package frontend

import "context"

// LastPlayedSourceReader returns source facts only after current item policy
// authorization. Nil means no trustworthy source facts are available. Facts
// describe the last durably recorded accepted sample; they can lag playback
// when asynchronous persistence fails and do not describe the current source.
type LastPlayedSourceReader interface {
	LastPlayedSource(context.Context, *Viewer, Device, RatingKey) (*PlaybackSourceHistory, error)
}
