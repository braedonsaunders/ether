package resolver

import (
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// On-demand stream links (core.Release.OpensOnDemand) are a Stremio
// add-on's play links: opening one makes the add-on resolve, download or
// import that release (a Usenet bridge queues and imports the NZB, a debrid
// add-on adds the torrent to its account). Validation therefore treats each
// open as the add-on's work, not a cheap probe:
//
//   - one on-demand check runs at a time, alone: no hedge, race or early
//     discovery check opens a second link beside it;
//   - the next on-demand candidate opens only after the current one failed;
//     a bounded open that times out is cancelled before trying the next
//     eligible candidate in the same Play;
//   - work nobody is waiting to watch (core.OnDemandStreamsAllowed) opens
//     none and stops at the first one, keeping rank order intact.
//
//   - except: a foreground check of a stream the add-on marks
//     debrid-cached (core.OnDemandCached) may be joined by one hedge after
//     onDemandHedgeDelay, since opening it queues nothing (ranked_window.go).
//
// Debrid torrents and Usenet releases are unaffected.

// onDemandBudget is how long one on-demand check may take: an add-on that
// prepares a release before answering is slower than a plain file, and
// giving up early only makes Ether open (and the add-on fetch) another one.
// It covers the stream probe's own deadline plus the health read.
const onDemandBudget = 22 * time.Second

var errOnDemandForeground = fmt.Errorf("add-on streams open only for Play: %w", core.ErrForegroundOnly)

// checkBudget is one candidate check's deadline.
func checkBudget(rel core.Release) time.Duration {
	if rel.OpensOnDemand() {
		return onDemandBudget
	}
	return candidateBudget
}

// onDemandHedgeDelay is how long a foreground check of a debrid-cached
// add-on stream runs alone before one more candidate (native, or another
// cached add-on stream) starts beside it. Opening a cached link costs the
// add-on no import, so a slow or dead lead fails over to a check that is
// already under way. Usenet-import and unmarked links stay sequential.
const onDemandHedgeDelay = 2 * time.Second
