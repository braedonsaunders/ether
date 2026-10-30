package playback

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func newTestResumeStore(t *testing.T) *resumeStore {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rs, err := newResumeStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.close(context.Background()) })
	return rs
}

// TestGCKeepsNZBOfRecordBeingWritten: one record drops the last committed
// reference to an NZB while another record for the same NZB found the file
// on disk but has not inserted its row yet. The file must survive, or the
// new row points at a deleted NZB.
func TestGCKeepsNZBOfRecordBeingWritten(t *testing.T) {
	ctx := context.Background()
	rs := newTestResumeStore(t)
	nzb := []byte("<nzb>shared</nzb>")
	rec := func(user int64, id string) resumeRecord {
		return resumeRecord{userID: user, key: 42, release: core.Release{ID: id}, updatedAt: time.Now()}
	}
	if err := rs.save(ctx, rec(1, "shared"), nzb); err != nil {
		t.Fatal(err)
	}
	// User 2's save of the same NZB is in flight: file found, row pending.
	lease, err := rs.acquireNZB(ctx, rec(2, "shared"), nzb)
	if err != nil {
		t.Fatal(err)
	}
	// User 1 switches release: the shared NZB loses its only committed row.
	if err := rs.save(ctx, rec(1, "other"), []byte("<nzb>other</nzb>")); err != nil {
		t.Fatal(err)
	}
	if err := rs.writeBundle(ctx, rec(2, "shared"), lease, nil, core.MediaInfo{}, nzb); err != nil {
		t.Fatal(err)
	}
	rs.releaseNZB(lease)
	if _, err := rs.readNZB(lease.hash); err != nil {
		t.Fatalf("NZB of user 2's record was deleted: %v", err)
	}
	// Once nothing references it, it is collected.
	if err := rs.delete(ctx, 2, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rs.nzbPath(lease.hash)); !os.IsNotExist(err) {
		t.Fatalf("unreferenced NZB kept: %v", err)
	}
}

// TestDeleteDropsKeyframeIndex: an expired resume record takes its
// keyframe index with it; nothing else would ever prune it.
func TestDeleteDropsKeyframeIndex(t *testing.T) {
	ctx := context.Background()
	rs := newTestResumeStore(t)
	r := resumeRecord{userID: 3, key: 77, release: core.Release{ID: "rel"}, updatedAt: time.Now()}
	if err := rs.put(ctx, r, []byte("<nzb/>"), []core.KeyframePoint{{Offset: 10, Time: time.Second}}); err != nil {
		t.Fatal(err)
	}
	if pts, err := rs.loadIndex(ctx, 3, 77, "rel"); err != nil || len(pts) != 1 {
		t.Fatalf("index before delete = %v, %v", pts, err)
	}
	if err := rs.delete(ctx, 3, 77); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rs.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_index`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d keyframe index rows left after delete (%v)", n, err)
	}
}
