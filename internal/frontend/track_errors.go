package frontend

import "errors"

// ErrTrackNotFound reports that an explicit selection does not identify one
// stream in the opened source.
var ErrTrackNotFound = errors.New("selected track is not available")

// ErrTrackSourceUnavailable reports that the broker cannot prove the opened
// source and its track layout across planning and playback admission.
var ErrTrackSourceUnavailable = errors.New("verified track source unavailable")
