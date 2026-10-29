package sessions

import (
	"testing"
	"time"
)

type testEntry struct {
	id  string
	key int
	at  time.Time
}

func TestTrackerMaintainsStableIDsAndExpires(t *testing.T) {
	now := time.Now()
	tr := NewTracker(func(id string, key int) testEntry { return testEntry{id: id, key: key} }, func(e *testEntry) time.Time { return e.at })
	tr.Ensure("cluster", 42)
	got := tr.Update("cluster", func(e *testEntry) { e.at = now })
	if got.id != "cluster" || got.key != 42 {
		t.Fatalf("cluster entry: %+v", got)
	}
	local := tr.Update("local", func(e *testEntry) { e.at = now.Add(-time.Minute) })
	if local.key != 1 {
		t.Fatalf("local sequence: %d", local.key)
	}
	expired := tr.Expire(now, 30*time.Second)
	if len(expired) != 1 || expired[0].id != "local" || tr.Has("local") || !tr.Has("cluster") {
		t.Fatalf("expired: %+v", expired)
	}
	removed, ok := tr.Remove("cluster")
	if !ok || removed.id != "cluster" || len(tr.List()) != 0 {
		t.Fatal("remove did not return entry")
	}
}
