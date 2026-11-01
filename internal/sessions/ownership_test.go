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
	e := o.entries[playbackOwnerKey{user: 1, client: "phone", key: core.MovieKey(550)}]
	if len(e.prior) != playbackOwnerHistoryLimit {
		t.Fatalf("history length=%d", len(e.prior))
	}
	if !o.stale(1, "phone", core.MovieKey(550), "id-998") || o.stale(1, "phone", core.MovieKey(550), "id-0") {
		t.Fatal("history did not retain newest and evict oldest")
	}
}

func TestOwnershipNamespacesCannotCollide(t *testing.T) {
	o := NewPlaybackOwners()
	now := time.Now()
	key := core.MovieKey(1)
	o.Bind(1, "client", key, "plex-a", "source-a", true, now)
	o.BindFor("broker", 1, "client", key, "other-a", "source-b", true, now)
	o.BindFor("broker", 1, "client", key, "other-b", "source-b", true, now)
	if o.Stale(1, "client", key, "plex-a") || !o.StaleFor("broker", 1, "client", key, "other-a") {
		t.Fatal("protocol ownership histories collided")
	}
}
