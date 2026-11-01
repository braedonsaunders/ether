package core

import (
	"context"
	"time"
)

// TrickplaySource identifies one playable source thumbnails can be
// extracted from: the release being streamed and the loopback URL its
// bytes are readable through (Playback.InputURL). Info carries the
// duration and, when known, the container keyframe index used to align
// extractions to keyframes.
type TrickplaySource struct {
	ReleaseID string
	InputURL  string
	Info      MediaInfo
	// AvailableUntil is the playback position already read by the client.
	// Generators must only fill intervals at or before this point and may
	// advance it on later calls as playback progresses.
	AvailableUntil time.Duration
}

// TrickplayProvider supplies on-the-fly preview thumbnails (Plex "BIF"
// indexes) for releases as they stream. It is an optional capability:
// callers reach it through a type assertion or an optional dependency and
// must tolerate its absence. Implementations only extract from sources
// handed to EnsureTrickplay (an active playback or warmup session); they
// never resolve or download a title just for thumbnails.
type TrickplayProvider interface {
	// EnsureTrickplay starts (or continues) progressive thumbnail
	// generation for the source in the background. It returns
	// immediately; duplicate calls for a release already generating are
	// no-ops.
	EnsureTrickplay(ctx context.Context, src TrickplaySource)
	// TrickplayAvailable reports whether thumbnails for the release
	// exist or are being generated, so a front end can advertise them
	// (Plex's indexes="sd").
	TrickplayAvailable(releaseID string) bool
	// TrickplayBIF returns a snapshot of the release's sparse BIF index
	// built from the frames generated so far.
	TrickplayBIF(releaseID string) ([]byte, bool)
	// TrickplayFrame returns the JPEG of the latest generated frame at
	// or before at (a fallback still when no real frame precedes it).
	TrickplayFrame(releaseID string, at time.Duration) ([]byte, bool)
	// SetTrickplayFallback records a low-fidelity still (TMDB episode
	// still or backdrop, already JPEG at roughly BIF width) used to fill
	// not-yet-generated intervals. Real frames replace it as they arrive.
	SetTrickplayFallback(releaseID string, jpeg []byte)
}

// TrickplayLeaser is supplied by the runtime-owned shared provider. Acquire
// registers a holder and starts generation atomically with idle cancellation.
// Calls return promptly; consumers retain one lease until their source ends.
type TrickplayLeaser interface {
	AcquireTrickplay(context.Context, TrickplaySource) (TrickplayLease, error)
}

// TrickplayLease holds one release. Ensure after Release is inert. Release is
// idempotent; cancelIfIdle applies the caller's idle policy only when no other
// holder remains. Only the runtime owner can Close the provider.
type TrickplayLease interface {
	Ensure(context.Context, TrickplaySource)
	Release(cancelIfIdle bool)
}
