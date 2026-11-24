package transcode

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Sequential prefetch while the previous GET is still on the wire must
// keep the empty Cluster. Serving the covering GOP again freezes the
// Plex tvOS timer (lab: 620–694 all 16MB copies of the 41:20 keyframe).
func TestMatroskaPrefetchKeepsEmptyCluster(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		dir:       dir,
		container: ContainerMatroska,
		segDur:    4 * time.Second,
		done:      []map[int]bool{{620: true, 621: true}},
		inflight:  map[int]int{620: 1},
		mkvDelivery: map[int]mkvDelivery{
			0: {segment: 620, seek: true},
		},
	}
	gop := filepath.Join(dir, segmentFile(620, "mkv"))
	alias := filepath.Join(dir, segmentFile(621, "mkv"))
	if err := os.WriteFile(gop, make([]byte, emptyClusterSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, emptyCluster(s.segStart(621)+matroskaOrigin), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, av := s.segmentFilesLocked(0, 621, http.MethodGet)
	if av != segmentReady {
		t.Fatalf("availability %d", av)
	}
	if paths[0] != alias {
		t.Fatalf("prefetch served %s, want empty cluster %s", paths[0], alias)
	}
}

// tvOS starts at 0/1 then seeks. The first media GET inside a GOP must
// still receive the covering keyframe, not the empty alias.
func TestMatroskaResumeServesCoveringGOP(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		dir:       dir,
		container: ContainerMatroska,
		segDur:    4 * time.Second,
		done:      []map[int]bool{{620: true, 621: true}},
		mkvDelivery: map[int]mkvDelivery{
			0: {segment: 1, seek: false},
		},
	}
	gop := filepath.Join(dir, segmentFile(620, "mkv"))
	alias := filepath.Join(dir, segmentFile(621, "mkv"))
	if err := os.WriteFile(gop, make([]byte, emptyClusterSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, emptyCluster(s.segStart(621)+matroskaOrigin), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, av := s.segmentFilesLocked(0, 621, http.MethodGet)
	if av != segmentReady {
		t.Fatalf("availability %d", av)
	}
	if paths[0] != gop {
		t.Fatalf("resume served %s, want covering GOP %s", paths[0], gop)
	}
}

// Finding the segment that covers a sequential fMP4 fetch must not read a
// full media segment into memory just to tell it from a small placeholder.
func TestFMP4CoveringSegmentSkipsReadingLargeSegment(t *testing.T) {
	s := &Session{
		dir:       t.TempDir(),
		container: ContainerFMP4,
		segDur:    4 * time.Second,
		done:      []map[int]bool{{0: true}},
	}
	const size = 8 << 20
	if err := os.WriteFile(s.trackSegPath(0, 0), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 20 {
		if k := s.coveringSegLocked(0, 0); k != 0 {
			t.Fatalf("coveringSegLocked = %d, want 0", k)
		}
	}
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("20 covering checks allocated %d bytes", alloc)
	if alloc >= size {
		t.Fatalf("20 covering checks of an 8 MiB segment allocated %d bytes, want under %d", alloc, size)
	}
}
