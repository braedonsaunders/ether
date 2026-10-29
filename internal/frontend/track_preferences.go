package frontend

import "context"

type TrackPreferences struct {
	AudioLanguages, SubtitleLanguages []string
	SubtitleMode                      string
}
type TrackPreferenceService interface {
	Get(context.Context, *Viewer) (TrackPreferences, error)
	SetSubtitle(context.Context, *Viewer, []string, string) error
}

// ScopedTrackPreferenceService is an optional extension for shared playback
// preferences; identifiers use the host's library and rating-key namespaces.
type ScopedTrackPreferenceService interface {
	GetScoped(context.Context, *Viewer, TrackScope) (TrackPreferences, int, int, string, map[string]string, error)
	SetScoped(context.Context, *Viewer, string, string, TrackPreferencePatch) error
	DeleteScoped(context.Context, *Viewer, string, string, string) error
	SetAudio(context.Context, *Viewer, []string) error
	SetSubtitleLanguage(context.Context, *Viewer, []string) error
	SetSubtitleMode(context.Context, *Viewer, string) error
	SetBitrate(context.Context, *Viewer, int) error
	SetHeight(context.Context, *Viewer, int, string) error
}

type TrackScope struct{ Device, Series, Library string }
type TrackPreferencePatch struct {
	AudioLanguages, SubtitleLanguages *[]string
	SubtitleMode                      *string
	MaxBitrateKbps                    *int
	MaxHeight                         *int
	PreferredQuality                  *string
}
