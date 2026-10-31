package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRepeatResumeWriteSkipsPayloadCommit(t *testing.T) {
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
	rec := resumeRecord{userID: 7, key: 42, release: core.Release{ID: "rel"}, profile: "p", size: 10, duration: time.Second, updatedAt: time.Unix(10, 0)}
	nzb := []byte("same-nzb")
	if err := rs.put(context.Background(), rec, nzb, nil); err != nil {
		t.Fatal(err)
	}
	var commits int
	rs.payload.afterOwnerCommit = func() error {
		commits++
		return nil
	}
	rec.updatedAt = time.Unix(99, 0)
	if err := rs.put(context.Background(), rec, nzb, nil); err != nil {
		t.Fatal(err)
	}
	if commits != 0 {
		t.Fatalf("unchanged resume committed %d times", commits)
	}
	row := &resumeRecord{userID: 7, key: 42, nzbHash: nzbHash(nzb)}
	first, err := rs.readResumeNZB(context.Background(), row)
	if err != nil || string(first) != string(nzb) || commits != 1 {
		t.Fatalf("first read %q err=%v commits=%d", first, err, commits)
	}
	second, err := rs.readResumeNZB(context.Background(), row)
	if err != nil || string(second) != string(nzb) || commits != 1 {
		t.Fatalf("cached read %q err=%v commits=%d", second, err, commits)
	}
	probe := core.MediaInfo{Container: "mp4", Size: 10, Duration: time.Second, Streams: []core.StreamInfo{{Type: "video", Codec: "h264"}}}
	if err := rs.putWithProbe(context.Background(), rec, nzb, nil, probe); err != nil {
		t.Fatal(err)
	}
	if commits != 2 {
		t.Fatalf("first real probe commits=%d", commits)
	}
	if err := rs.putWithProbe(context.Background(), rec, nzb, nil, probe); err != nil {
		t.Fatal(err)
	}
	if commits != 2 {
		t.Fatalf("stored probe committed again: %d", commits)
	}
}

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
