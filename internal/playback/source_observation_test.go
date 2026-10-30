package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSourceSnapshotSurvivesFailoverAndStop(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "history-snapshot")
	first, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || first.Revision == "" || first.ReleaseID != "A" {
		t.Fatalf("first source: %+v %v", first, err)
	}
	if _, err = h.m.Failover(context.Background(), ps.ID, 0); err != nil {
		t.Fatal(err)
	}
	second, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || second.ReleaseID != "B" || first.Revision == second.Revision || first.ReleaseID != "A" {
		t.Fatalf("source snapshots: %+v %+v %v", first, second, err)
	}
	h.m.Stop(ps.ID)
	if _, err = h.m.CurrentSource(context.Background(), ps.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("retired source: %v", err)
	}
}
