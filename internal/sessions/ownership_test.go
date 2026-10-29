package sessions

import (
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

func TestIssue94OwnerHistoryBound(t *testing.T) {
	o := newPlaybackOwners()
	now := time.Now()
	for i := 0; i < 1000; i++ {
		o.bind(1, "phone", core.MovieKey(550), fmt.Sprintf("id-%d", i), "play", true, now)
	}
	e := o.entries[playbackOwnerKey{1, "phone", core.MovieKey(550)}]
	if len(e.prior) != playbackOwnerHistoryLimit {
		t.Fatalf("history length=%d", len(e.prior))
	}
	if !o.stale(1, "phone", core.MovieKey(550), "id-998") || o.stale(1, "phone", core.MovieKey(550), "id-0") {
		t.Fatal("history did not retain newest and evict oldest")
	}
}
