package segcache_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/blobcache"
	"github.com/braedonsaunders/ether/internal/segcache"
	"github.com/braedonsaunders/ether/internal/store"
)

type mediaNode struct {
	seg   *segcache.Cache
	files *blobcache.Cache
	games *blobcache.Namespace
}

func startMediaNode(t *testing.T, dir string, db *sql.DB, id string) mediaNode {
	t.Helper()
	return startTimedNode(t, dir, db, id, time.Now)
}

func startTimedNode(t *testing.T, dir string, db *sql.DB, id string, fileClock func() time.Time) mediaNode {
	t.Helper()
	var seg *segcache.Cache
	var err error
	if db == nil {
		seg, err = segcache.Open(dir, 1000, nil)
	} else {
		seg, err = segcache.OpenShared(dir, 1000, db, id, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seg.Close() })
	files, err := blobcache.Open(filepath.Join(dir, "plugin-files"), blobcache.Options{Now: fileClock, Unified: true, Shared: db != nil, SweepPeriod: -1, PoolCapacity: seg.MediaFileCapacity, MakeFileRoom: seg.MakeFileRoom, CheckVolume: seg.CheckVolume})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seg.SetFilePool(nil); files.Close() })
	seg.SetFilePool(files)
	return mediaNode{seg, files, files.Namespace("games", blobcache.Quota{})}
}

func TestMediaPoolMixedAdmission(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			var db *sql.DB
			if mode == "shared" {
				var err error
				db, err = store.OpenMemory(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
			}
			dir := t.TempDir()
			a := startMediaNode(t, dir, db, "a")
			b := a
			if db != nil {
				b = startMediaNode(t, dir, db, "b")
			}
			if err := a.seg.Put("movie", 0, make([]byte, 500)); err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte{0xA5}, 700)
			ref, err := b.games.Put(context.Background(), "disc.iso", bytes.NewReader(payload), int64(len(payload)))
			if err != nil {
				t.Fatal(err)
			}
			if a.seg.Has("movie", 0) {
				t.Fatal("eligible segments remained above budget")
			}
			game, err := a.games.Open(ref)
			if err != nil {
				t.Fatal(err)
			}
			defer game.Close()
			if u, budget := a.seg.Usage(); u != 700 || budget != 1000 {
				t.Fatalf("usage %d/%d", u, budget)
			}
			if err := b.seg.Put("too-large-with-reader", 0, make([]byte, 500)); err == nil {
				t.Fatal("active disc reader lost its protection")
			}
			b.games.Purge()
			if _, ok := b.games.LookupKey("disc.iso"); !ok {
				t.Fatal("clear removed a file during a read")
			}
			got, err := io.ReadAll(game)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("peer disc read: %v", err)
			}
			game.Close()
			if err := a.seg.Put("movie2", 0, make([]byte, 500)); err != nil {
				t.Fatal(err)
			}
			if _, ok := b.games.LookupKey("disc.iso"); ok {
				t.Fatal("idle disc was not evicted")
			}
			if err := a.seg.Pin("movie2", "owner"); err != nil {
				t.Fatal(err)
			}
			p, err := b.games.OpenPartial(context.Background(), "next-disc")
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err := p.Reserve(600); err == nil {
				t.Fatal("pinned segments lost budget protection")
			}
			if err := p.Reserve(400); err != nil {
				t.Fatal(err)
			}
			if err := a.seg.Put("late", 0, make([]byte, 200)); err == nil {
				t.Fatal("segment spent outstanding file reservation")
			}
			if !a.seg.Has("movie2", 0) {
				t.Fatal("pinned movie disappeared")
			}
		})
	}
}

// The volume floor can impose a tighter bound than the configured budget.
// A pinned playback release survives while an idle library file yields space.
func TestMediaPoolVolumeFloor(t *testing.T) {
	node := startMediaNode(t, t.TempDir(), nil, "a")
	var free atomic.Int64
	free.Store(1500)
	node.seg.SetDiskUsage(func() (int64, int64, error) { return 10000, free.Load(), nil })
	if err := node.seg.Pin("playing", "session"); err != nil {
		t.Fatal(err)
	}
	if err := node.seg.Put("playing", 0, make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	_, err := node.games.Put(context.Background(), "disc", bytes.NewReader(make([]byte, 300)), 300)
	if err != nil {
		t.Fatal(err)
	}
	free.Store(550)
	if err := node.seg.Put("next", 0, make([]byte, 250)); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.games.LookupKey("disc"); ok {
		t.Fatal("free space floor did not reclaim idle library file")
	}
	if err := node.seg.ClearUnpinned("playing"); err == nil {
		t.Fatal("clear removed a pinned release")
	}
	if !node.seg.Has("playing", 0) {
		t.Fatal("space pressure displaced a pinned release")
	}
}

func TestMediaPoolConcurrentStoreAdmissions(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir := t.TempDir()
	a := startMediaNode(t, dir, db, "a")
	b := startMediaNode(t, dir, db, "b")
	if err := a.seg.Pin("playing", "session"); err != nil {
		t.Fatal(err)
	}
	partial, err := b.games.OpenPartial(ctx, "fetching")
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()

	start := make(chan struct{})
	segment, file := make(chan error, 1), make(chan error, 1)
	go func() { <-start; segment <- a.seg.Put("playing", 0, make([]byte, 600)) }()
	go func() { <-start; file <- partial.Reserve(600) }()
	close(start)
	segmentErr, fileErr := <-segment, <-file
	if (segmentErr == nil) == (fileErr == nil) {
		t.Fatalf("one of two claims must fit: segment=%v file=%v", segmentErr, fileErr)
	}
}

func TestMediaPoolAccountsForSegmentsOnRestart(t *testing.T) {
	dir := t.TempDir()
	old, err := segcache.Open(dir, 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Pin("pinned", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := old.Put("pinned", 0, make([]byte, 700)); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	node := startMediaNode(t, dir, nil, "a")
	_, err = node.games.Put(context.Background(), "disc", bytes.NewReader(make([]byte, 500)), 500)
	if err == nil {
		t.Fatal("unindexed pinned segments were omitted from admission")
	}
	if err := node.seg.Put("too-large", 0, make([]byte, 500)); err == nil {
		t.Fatal("segment claimed bytes held before restart")
	}
	if used, _ := node.seg.Usage(); used != 700 {
		t.Fatalf("usage after restart %d, want 700", used)
	}
}

func TestMediaPoolEvictsOldestAcrossStores(t *testing.T) {
	for _, backend := range []string{"local", "shared"} {
		for _, first := range []string{"file", "segment"} {
			for _, pressure := range []string{"segment admission", "file admission", "smaller budget", "volume floor"} {
				t.Run(backend+"/"+first+"/"+pressure, func(t *testing.T) {
					var db *sql.DB
					if backend == "shared" {
						var err error
						db, err = store.OpenMemory(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { db.Close() })
					}
					dir := t.TempDir()
					at := time.Now().Add(time.Hour)
					if first == "file" {
						at = at.Add(-2 * time.Hour)
					}
					clock := func() time.Time { return at }
					a := startTimedNode(t, dir, db, "a", clock)
					b := a
					if db != nil {
						b = startTimedNode(t, dir, db, "b", clock)
					}
					putFile := func(name string, size int) {
						t.Helper()
						if _, err := a.games.Put(context.Background(), name, bytes.NewReader(make([]byte, size)), int64(size)); err != nil {
							t.Fatal(err)
						}
					}
					putFile("disc", 350)
					if err := a.seg.Put("release", 0, make([]byte, 350)); err != nil {
						t.Fatal(err)
					}
					switch pressure {
					case "segment admission":
						if err := b.seg.Put("new stream", 0, make([]byte, 450)); err != nil {
							t.Fatal(err)
						}
					case "file admission":
						putFile("new disc", 450)
					case "smaller budget":
						b.seg.SetBudget(500)
					case "volume floor":
						b.seg.SetDiskUsage(func() (int64, int64, error) { return 10000, 300, nil })
					}
					_, discRemains := a.games.LookupKey("disc")
					streamRemains := a.seg.Has("release", 0)
					if discRemains != (first == "segment") || streamRemains != (first == "file") {
						t.Fatalf("oldest %s: retained disc=%v, stream=%v", first, discRemains, streamRemains)
					}
				})
			}
		}
	}
}

func TestMediaPoolAdaptiveReplaySurvivesNewerIdleFile(t *testing.T) {
	for _, backend := range []string{"local", "shared"} {
		t.Run(backend, func(t *testing.T) {
			var db *sql.DB
			if backend == "shared" {
				var err error
				db, err = store.OpenMemory(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
			}
			node := startTimedNode(t, t.TempDir(), db, "a", func() time.Time { return time.Now().Add(time.Hour) })
			node.seg.SetPolicy(segcache.Policy{Mode: "adaptive", ReplayBoost: 2 * time.Hour})
			if err := node.seg.Put("replay", 0, make([]byte, 400)); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				node.seg.RecordPlay("replay", "Film", 400)
			}
			if _, err := node.games.Put(context.Background(), "disc", bytes.NewReader(make([]byte, 400)), 400); err != nil {
				t.Fatal(err)
			}
			if err := node.seg.Put("another", 0, make([]byte, 300)); err != nil {
				t.Fatal(err)
			}
			if !node.seg.Has("replay", 0) {
				t.Fatal("adaptive replay preference was lost under mixed pressure")
			}
			if _, ok := node.games.LookupKey("disc"); ok {
				t.Fatal("idle disc retained ahead of replay")
			}
		})
	}
}

func TestMediaPoolCallbackCanReadUsage(t *testing.T) {
	node := startMediaNode(t, t.TempDir(), nil, "a")
	node.seg.SetDiskUsage(func() (int64, int64, error) {
		used, _ := node.seg.Usage()
		return 10000, 2000 - used, nil
	})
	if _, err := node.games.Put(context.Background(), "disc", bytes.NewReader(make([]byte, 700)), 700); err != nil {
		t.Fatal(err)
	}
	if err := node.seg.Put("stream", 0, make([]byte, 500)); err != nil {
		t.Fatal(err)
	}
	if used, _ := node.seg.Usage(); used != 500 {
		t.Fatalf("usage %d", used)
	}
}

func TestMediaPoolSharedSubsecondOrdering(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	base := time.Now().Truncate(time.Second)
	node := startTimedNode(t, t.TempDir(), db, "a", func() time.Time { return base.Add(50 * time.Millisecond) })
	if err := node.seg.Put("stream", 0, make([]byte, 350)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE segcache_segments SET touched=?,touched_nanos=? WHERE release_id='stream'", base.Unix(), 90000000); err != nil {
		t.Fatal(err)
	}
	if _, err = node.games.Put(ctx, "disc", bytes.NewReader(make([]byte, 350)), 350); err != nil {
		t.Fatal(err)
	}
	node.seg.SetBudget(500)
	if !node.seg.Has("stream", 0) {
		t.Fatal("a newer segment was rounded below the disc's last-use timestamp")
	}
}

type countingFilePool struct {
	segcache.FilePool
	allocations atomic.Int32
}

func (p *countingFilePool) WithAllocation(fn func(accounted, unwritten int64, files segcache.FileCandidates) error) error {
	p.allocations.Add(1)
	return p.FilePool.WithAllocation(fn)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMediaPoolBatchedReservationAndPoolFullWarn(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var db *sql.DB
			if mode == "shared" {
				var err error
				db, err = store.OpenMemory(ctx)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
			}
			dir := t.TempDir()
			var logBuf syncBuffer
			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			var seg *segcache.Cache
			var err error
			if db == nil {
				seg, err = segcache.Open(dir, 1000, logger)
			} else {
				seg, err = segcache.OpenShared(dir, 1000, db, "a", logger)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { seg.Close() })
			files, err := blobcache.Open(filepath.Join(dir, "plugin-files"), blobcache.Options{
				Unified:      true,
				Shared:       db != nil,
				SweepPeriod:  -1,
				PoolCapacity: seg.MediaFileCapacity,
				MakeFileRoom: seg.MakeFileRoom,
				CheckVolume:  seg.CheckVolume,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { seg.SetFilePool(nil); files.Close() })
			pool := &countingFilePool{FilePool: files}
			seg.SetFilePool(pool)

			var diskCalls atomic.Int32
			seg.SetDiskUsage(func() (int64, int64, error) {
				diskCalls.Add(1)
				return 10000, 5000, nil
			})
			pool.allocations.Store(0)

			// A burst of 10 small segments with ample headroom should take the
			// pool lock and measure disk free space only once.
			for i := 0; i < 10; i++ {
				if err := seg.Put("burst", i, make([]byte, 20)); err != nil {
					t.Fatalf("put %d: %v", i, err)
				}
			}
			if got := pool.allocations.Load(); got != 1 {
				t.Fatalf("WithAllocation calls = %d, want 1", got)
			}
			if got := diskCalls.Load(); got != 1 {
				t.Fatalf("diskFn calls = %d, want 1", got)
			}
			if db != nil {
				var rows int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM segcache_writes`).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows > 1 {
					t.Fatalf("segcache_writes rows = %d, want <= 1 batched row", rows)
				}
			}

			// A subsequent file admission must reclaim any unspent segment batch
			// headroom so the full remaining budget is available.
			games := files.Namespace("games", blobcache.Quota{})
			if _, err := games.Put(ctx, "disc.iso", bytes.NewReader(make([]byte, 800)), 800); err != nil {
				t.Fatalf("file put after segment batch: %v", err)
			}
			if db != nil {
				var rows int
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM segcache_writes`).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows != 0 {
					t.Fatalf("segcache_writes rows after file admission = %d, want 0", rows)
				}
			}

			// Pin the segments and hold the file open so the pool cannot evict
			// anything; repeated ErrPoolFull failures must log at Warn once per interval.
			if err := seg.Pin("burst", "session"); err != nil {
				t.Fatal(err)
			}
			ref, ok := games.LookupKey("disc.iso")
			if !ok {
				t.Fatal("missing disc.iso")
			}
			rc, err := games.Open(ref)
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()

			for i := 0; i < 4; i++ {
				err := seg.Put("overflow", i, make([]byte, 100))
				if !errors.Is(err, segcache.ErrPoolFull) {
					t.Fatalf("overflow put %d = %v, want ErrPoolFull", i, err)
				}
			}
			logs := logBuf.String()
			if count := strings.Count(logs, "media cache full"); count != 1 {
				t.Fatalf("ErrPoolFull warn count = %d, want 1; logs:\n%s", count, logs)
			}
		})
	}
}

// Unused shared batch headroom goes back to the pool once the batch window
// ends, even when no further segment write arrives to release it.
func TestSharedSegmentBatchReleasedAfterWindow(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir := t.TempDir()
	seg, err := segcache.OpenShared(dir, 1000, db, "a", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seg.Close() })
	files, err := blobcache.Open(filepath.Join(dir, "plugin-files"), blobcache.Options{
		Unified: true, Shared: true, SweepPeriod: -1,
		PoolCapacity: seg.MediaFileCapacity, MakeFileRoom: seg.MakeFileRoom, CheckVolume: seg.CheckVolume,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seg.SetFilePool(nil); files.Close() })
	seg.SetFilePool(files)
	seg.SetDiskUsage(func() (int64, int64, error) { return 10000, 5000, nil })
	if err := seg.Put("burst", 0, make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	reserved := func() int64 {
		var n sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT SUM(bytes) FROM segcache_writes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n.Int64
	}
	if reserved() == 0 {
		t.Fatal("expected a batched reservation after the first segment")
	}
	deadline := time.Now().Add(3 * time.Second)
	for reserved() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("batch headroom still reserved: %d bytes", reserved())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
