package model

// TrackPreferences contains the language controls on series and season details.
type TrackPreferences struct {
	Setting []Setting `plex:"Setting"`
}
