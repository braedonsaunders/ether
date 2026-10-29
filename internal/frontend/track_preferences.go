package frontend

import "context"

type TrackPreferences struct {
	AudioLanguages, SubtitleLanguages []string
	SubtitleMode                      string
}
type TrackPreferenceService interface {
	Get(context.Context, *Viewer) (TrackPreferences, error)
	Set(context.Context, *Viewer, TrackPreferences) error
}
