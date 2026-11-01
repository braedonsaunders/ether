package core

import (
	"context"
	"time"
)

// Reasons for PrewarmOptions.Reason.
const (
	PrewarmDetail   = "detail"   // a detail page was opened
	PrewarmTrailer  = "trailer"  // a trailer or Cinema Trailer for the title is playing
	PrewarmPostPlay = "postplay" // the viewer is near the end of something and this is up next
	PrewarmBrowse   = "browse"   // a home/section row is on screen; open the debrid link only
)

// PrewarmOptions tune a speculative warm-up of a title before the viewer
// presses Play.
type PrewarmOptions struct {
	// Reason labels the trigger for logs and budgets (Prewarm* constants).
	Reason string `json:"reason"`
	// PrefetchBytes is how much of the opening of the file to pre-buffer at
	// background priority once the release is resolved, opened and probed.
	// Zero uses the implementation's default for the reason.
	PrefetchBytes int64 `json:"prefetchBytes,omitempty"`
	// DuringPlayback allows the warm-up while the viewer already has an
	// active session (a trailer, or the end of a film, is playing).
	DuringPlayback bool `json:"duringPlayback,omitempty"`
}

// Prewarmer is implemented by playback managers that can resolve, open,
// probe and pre-buffer a title ahead of Play, so the later Prepare for the
// same user, client and title is near-instant. A warm-up never reserves a
// stream or writes resume/history records. It reports whether a warm-up was
// scheduled (false when skipped by budgets or already warm).
type Prewarmer interface {
	Prewarm(ctx context.Context, user *User, key RatingKey, caps ClientCaps, opts PrewarmOptions) bool
}

// WarmProber is optionally implemented by playback managers that complete a
// speculative probe (Prewarm/PreResolve) before Play. WarmProbeInfo returns
// the probed media info a warm-up already produced for this user/client/key,
// so metadata and play-queue renders can list the file's real audio and
// subtitle tracks instead of the single invented audio track of the
// estimate. ok=false when no warm probe has completed (or it has expired).
// It never starts a probe or blocks on one in flight.
//
// It complements ResumedTracks, which only covers a viewer's later opens:
// this covers the first open, once the detail-page warm-up has landed.
type WarmProber interface {
	WarmProbeInfo(user *User, caps ClientCaps, key RatingKey) (MediaInfo, bool)
}

// WarmProbeWaiter is optionally implemented by playback managers that can wait
// up to a bounded budget for an already-scheduled warm probe (PreResolve) to
// finish, so the first detail-page open can render the file's real audio and
// subtitle streams instead of an estimate. It never cancels the background
// warm-up when the budget expires.
type WarmProbeWaiter interface {
	WaitWarmProbe(ctx context.Context, user *User, caps ClientCaps, key RatingKey, budget time.Duration) (MediaInfo, bool)
}
