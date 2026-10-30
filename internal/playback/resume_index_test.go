package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestResumeKeyframeIndexBoundedAndPersisted(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs, err := newResumeStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.close(context.Background()) })
	want := []core.KeyframePoint{{Offset: 400, Time: 40 * time.Second}}
	if err := rs.saveIndex(context.Background(), 7, 42, "release", want); err != nil {
		t.Fatal(err)
	}
	got, err := rs.loadIndex(context.Background(), 7, 42, "release")
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("loaded index = %+v, %v", got, err)
	}
	points := make([]core.KeyframePoint, maxResumeKeyframes+1)
	for i := range points {
		points[i] = core.KeyframePoint{Offset: int64(i + 1), Time: time.Duration(i + 1)}
	}
	if err := rs.saveIndex(context.Background(), 7, 43, "bounded", points); err != nil {
		t.Fatal(err)
	}
	got, err = rs.loadIndex(context.Background(), 7, 43, "bounded")
	if err != nil || len(got) != maxResumeKeyframes {
		t.Fatalf("bounded index length = %d, %v", len(got), err)
	}
}
