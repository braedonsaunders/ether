package frontend

import (
	"context"
	"time"
)

// AudioFallback streams one source audio track independently of direct video.
// It uses the shared source and HLS segmenter, never resolves another release.
type AudioFallback interface {
	AudioFallbackAvailable() bool
	StartAudioFallback(context.Context, SessionAccess, string, int, time.Duration) (AudioFallbackResult, error)
	StopAudioFallback(context.Context, SessionAccess, string, string) error
}
type AudioFallbackResult struct {
	ID         string
	Generation uint64
	URL        string
}
