//go:build plexparity

package livetv

import (
	"fmt"
	"sync/atomic"
)

// parityLiveSessionID is used only by the staged parity binaries. Keeping the
// sequence per process also stabilizes media IDs derived from the session ID.
var parityLiveSequence atomic.Uint64

func parityLiveSessionID() string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", parityLiveSequence.Add(1))
}
