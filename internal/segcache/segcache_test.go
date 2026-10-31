package segcache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openT(t testing.TB, dir string, budget int64) *Cache {
	t.Helper()
	c, err := Open(dir, budget, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c.touchEvery = 0
	c.protectTTL = 0
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// payload builds deterministic, self-describing data: an 8-byte version
// followed by bytes derived from (id, seg, version).
func payload(id string, seg, version, size int) []byte {
	if size < 8 {
		size = 8
	}
	b := make([]byte, size)
	binary.LittleEndian.PutUint64(b, uint64(version))
	seed := uint64(seg)*1315423911 + uint64(version)*2654435761 + uint64(len(id))
	for _, ch := range id {
		seed = seed*31 + uint64(ch)
	}
	for i := 8; i < size; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = byte(seed >> 56)
	}
	return b
}

func mustPut(t testing.TB, c *Cache, id string, seg int, data []byte) {
	t.Helper()
	if err := c.Put(id, seg, data); err != nil {
		t.Fatalf("Put(%q,%d): %v", id, seg, err)
	}
}

func segPath(c *Cache, id string, seg int) string {
	return filepath.Join(c.dataDir, releaseDirRel(id), segName(seg))
}

// checkInvariants verifies the in-memory index is self-consistent.
func checkInvariants(t testing.TB, c *Cache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var used int64
	n := 0
	walk := func(l *lruList, parked bool) {
		cnt := 0
		var prev *entry
		for e := l.head; e != nil; e = e.next {
			if e.prev != prev {
				t.Errorf("broken prev link at %q/%d", e.rel.id, e.seg)
			}
			if e.parked != parked {
				t.Errorf("entry %q/%d parked=%v in wrong list", e.rel.id, e.seg, e.parked)
			}
			if c.rels[e.rel.id] != e.rel || e.rel.segs[e.seg] != e {
				t.Errorf("listed entry %q/%d not in index", e.rel.id, e.seg)
			}
			used += e.size
			cnt++
			prev = e
		}
		if l.tail != prev {
			t.Errorf("tail mismatch")
		}
		if cnt != l.n {
			t.Errorf("list n=%d, counted %d", l.n, cnt)
		}
		n += cnt
	}
	walk(&c.lru, false)
	walk(&c.parked, true)
	if used != c.used {
		t.Errorf("used=%d, sum of entries=%d", c.used, used)
	}
	if n != c.segCount {
		t.Errorf("segCount=%d, listed=%d", c.segCount, n)
	}
	idx := 0
	for _, r := range c.rels {
		var b int64
		for _, e := range r.segs {
			b += e.size
		}
		if b != r.bytes {
			t.Errorf("release %q bytes=%d, sum=%d", r.id, r.bytes, b)
		}
		idx += len(r.segs)
	}
	if idx != n {
		t.Errorf("index has %d entries, lists %d", idx, n)
	}
}

// diskUsage sums payload sizes of all segment files on disk.
func diskUsage(t testing.TB, c *Cache) (int64, int) {
	t.Helper()
	var used int64
	n := 0
	err := filepath.WalkDir(c.dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if _, ok := parseSegName(d.Name()); ok && !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			used += info.Size() - headerSize
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return used, n
}

func TestPutGetHasRoundTrip(t *testing.T) {
	c := openT(t, t.TempDir(), 0)
	ids := []string{"plain", "a/b:c", "日本語 リリース", "../../etc/passwd", strings.Repeat("x", 500)}
	for i, id := range ids {
		for seg := range 3 {
			mustPut(t, c, id, seg, payload(id, seg, i, 1000+seg))
		}
	}
	for i, id := range ids {
		for seg := range 3 {
			if !c.Has(id, seg) {
				t.Fatalf("Has(%q,%d)=false", id, seg)
			}
			got, ok := c.Get(id, seg)
			if !ok || !bytes.Equal(got, payload(id, seg, i, 1000+seg)) {
				t.Fatalf("Get(%q,%d) ok=%v mismatch", id, seg, ok)
			}
		}
		if c.Has(id, 3) {
			t.Fatalf("Has(%q,3) should be false", id)
		}
		if _, ok := c.Get(id, 3); ok {
			t.Fatalf("Get(%q,3) should miss", id)
		}
	}
	if _, ok := c.Get("missing", 0); ok {
		t.Fatal("Get missing release should miss")
	}
	// Empty payloads are valid.
	mustPut(t, c, "empty", 0, nil)
	if got, ok := c.Get("empty", 0); !ok || len(got) != 0 {
		t.Fatalf("empty payload: ok=%v len=%d", ok, len(got))
	}
	if err := c.Put("", 0, []byte("x")); err == nil {
		t.Fatal("empty release id accepted")
	}
	if err := c.Put("x", -1, []byte("x")); err == nil {
		t.Fatal("negative segment accepted")
	}
	checkInvariants(t, c)
}

func TestReplaceAndUsage(t *testing.T) {
	c := openT(t, t.TempDir(), 0)
	mustPut(t, c, "r", 0, payload("r", 0, 1, 1000))
	mustPut(t, c, "r", 1, payload("r", 1, 1, 500))
	if used, budget := c.Usage(); used != 1500 || budget != 0 {
		t.Fatalf("Usage=%d,%d want 1500,0", used, budget)
	}
	mustPut(t, c, "r", 0, payload("r", 0, 2, 200))
	if used, _ := c.Usage(); used != 700 {
		t.Fatalf("after replace used=%d want 700", used)
	}
	got, ok := c.Get("r", 0)
	if !ok || !bytes.Equal(got, payload("r", 0, 2, 200)) {
		t.Fatal("replace not visible")
	}
	rs := c.Releases()
	if len(rs) != 1 || rs[0].ReleaseID != "r" || rs[0].Bytes != 700 || rs[0].Segments != 2 || rs[0].LastUsed.IsZero() {
		t.Fatalf("Releases=%+v", rs)
	}
	if du, n := diskUsage(t, c); du != 700 || n != 2 {
		t.Fatalf("disk usage=%d n=%d", du, n)
	}
	checkInvariants(t, c)
}

func TestLRUEvictionOrder(t *testing.T) {
	const sz = 100
	c := openT(t, t.TempDir(), 3*sz)
	for seg := range 3 {
		mustPut(t, c, "r", seg, payload("r", seg, 0, sz))
	}
	if _, ok := c.Get("r", 0); !ok { // refresh 0: LRU order is now 1, 2, 0
		t.Fatal("miss")
	}
	c.Has("r", 1) // Has must not refresh
	mustPut(t, c, "r", 3, payload("r", 3, 0, sz))
	if c.Has("r", 1) {
		t.Fatal("segment 1 should have been evicted")
	}
	for _, s := range []int{0, 2, 3} {
		if !c.Has("r", s) {
			t.Fatalf("segment %d evicted", s)
		}
	}
	if _, err := os.Stat(segPath(c, "r", 1)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("evicted file still on disk: %v", err)
	}
	mustPut(t, c, "r", 4, payload("r", 4, 0, 2*sz)) // evicts 2 then 0
	if c.Has("r", 2) || c.Has("r", 0) || !c.Has("r", 3) || !c.Has("r", 4) {
		t.Fatal("wrong eviction for double-size put")
	}
	if used, _ := c.Usage(); used != 3*sz {
		t.Fatalf("used=%d", used)
	}
	if du, _ := diskUsage(t, c); du != 3*sz {
		t.Fatalf("disk usage=%d", du)
	}
	checkInvariants(t, c)
}

func TestTooLarge(t *testing.T) {
	c := openT(t, t.TempDir(), 100)
	err := c.Put("r", 0, make([]byte, 101))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err=%v want ErrTooLarge", err)
	}
	if c.Has("r", 0) {
		t.Fatal("oversize segment stored")
	}
	mustPut(t, c, "r", 0, make([]byte, 100)) // exactly the budget is fine
	if used, _ := c.Usage(); used != 100 {
		t.Fatalf("used=%d", used)
	}
}

func TestPinnedNeverEvicted(t *testing.T) {
	const sz = 100
	c := openT(t, t.TempDir(), 4*sz)
	for seg := range 3 {
		mustPut(t, c, "pinned", seg, payload("pinned", seg, 0, sz))
	}
	if err := c.Pin("pinned", "user"); err != nil {
		t.Fatal(err)
	}
	if err := c.Pin("pinned", "in-progress"); err != nil {
		t.Fatal(err)
	}
	for seg := range 10 {
		mustPut(t, c, "other", seg, payload("other", seg, 0, sz))
		checkInvariants(t, c)
	}
	for seg := range 3 {
		if !c.Has("pinned", seg) {
			t.Fatalf("pinned segment %d evicted", seg)
		}
	}
	if used, _ := c.Usage(); used != 4*sz {
		t.Fatalf("used=%d", used)
	}
	// Everything pinned: allowed to go over budget.
	if err := c.Pin("other", "user"); err != nil {
		t.Fatal(err)
	}
	mustPut(t, c, "pinned", 3, payload("pinned", 3, 0, sz))
	mustPut(t, c, "pinned", 4, payload("pinned", 4, 0, sz))
	if used, _ := c.Usage(); used != 6*sz {
		t.Fatalf("over-budget used=%d want %d", used, 6*sz)
	}
	if got := c.Pins(); !slices.Equal(got["pinned"], []string{"in-progress", "user"}) || !slices.Equal(got["other"], []string{"user"}) {
		t.Fatalf("Pins=%v", got)
	}
	// One reason left: still pinned.
	if err := c.Unpin("pinned", "user"); err != nil {
		t.Fatal(err)
	}
	if used, _ := c.Usage(); used != 6*sz {
		t.Fatalf("used=%d after partial unpin", used)
	}
	// Last reason gone: evicted down to budget immediately.
	if err := c.Unpin("pinned", "in-progress"); err != nil {
		t.Fatal(err)
	}
	if used, _ := c.Usage(); used != 4*sz {
		t.Fatalf("used=%d after unpin, want %d", used, 4*sz)
	}
	if c.Has("pinned", 0) {
		t.Fatal("unpinned oldest segment not evicted")
	}
	if _, ok := c.Pins()["pinned"]; ok {
		t.Fatal("pin entry not removed")
	}
	if err := c.Unpin("nope", "user"); err != nil {
		t.Fatalf("unpin unknown: %v", err)
	}
	if err := c.Pin("", "user"); err == nil {
		t.Fatal("empty release pin accepted")
	}
	checkInvariants(t, c)
}

func TestProtectCallback(t *testing.T) {
	const sz = 100
	c := openT(t, t.TempDir(), 3*sz)
	var calls atomic.Int32
	var mu sync.Mutex
	protected := map[string]bool{"a": true}
	c.Protect(func() map[string]bool {
		calls.Add(1)
		mu.Lock()
		defer mu.Unlock()
		return protected // the cache must copy, not retain, this map
	})
	mustPut(t, c, "a", 0, payload("a", 0, 0, sz))
	mustPut(t, c, "a", 1, payload("a", 1, 0, sz))
	mustPut(t, c, "b", 0, payload("b", 0, 0, sz))
	if n := calls.Load(); n != 0 {
		t.Fatalf("callback called %d times without eviction", n)
	}
	mustPut(t, c, "b", 1, payload("b", 1, 0, sz))
	if calls.Load() == 0 {
		t.Fatal("callback not called during eviction")
	}
	if !c.Has("a", 0) || !c.Has("a", 1) || c.Has("b", 0) || !c.Has("b", 1) {
		t.Fatal("protected release evicted or wrong victim")
	}
	// Protection lifted: next eviction considers "a" again (oldest first).
	mu.Lock()
	protected = map[string]bool{}
	mu.Unlock()
	mustPut(t, c, "b", 2, payload("b", 2, 0, sz))
	if c.Has("a", 0) || !c.Has("a", 1) || !c.Has("b", 1) || !c.Has("b", 2) {
		t.Fatal("expected a/0 evicted after protection lifted")
	}
	c.Protect(nil)
	mustPut(t, c, "b", 3, payload("b", 3, 0, sz))
	if used, _ := c.Usage(); used != 3*sz {
		t.Fatalf("used=%d", used)
	}
	checkInvariants(t, c)
}

func TestProtectResultCached(t *testing.T) {
	c := openT(t, t.TempDir(), 100)
	c.protectTTL = time.Hour
	var calls atomic.Int32
	c.Protect(func() map[string]bool { calls.Add(1); return nil })
	for seg := range 10 {
		mustPut(t, c, "r", seg, payload("r", seg, 0, 100))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("callback called %d times, want 1 (cached)", n)
	}
}

func TestPinsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	c := openT(t, dir, 0)
	for _, p := range [][2]string{{"a/b", "user"}, {"a/b", "in-progress"}, {"ünï", "user"}, {"gone", "user"}} {
		if err := c.Pin(p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Unpin("gone", "user"); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := c.Pin("x", "y"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Pin after Close: %v", err)
	}
	c2 := openT(t, dir, 0)
	got := c2.Pins()
	want := map[string][]string{"a/b": {"in-progress", "user"}, "ünï": {"user"}}
	if len(got) != len(want) {
		t.Fatalf("Pins=%v want %v", got, want)
	}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			t.Fatalf("Pins=%v want %v", got, want)
		}
	}
	// Pinned releases without segments are still listed for the admin UI.
	rs := c2.Releases()
	if len(rs) != 2 {
		t.Fatalf("Releases=%+v", rs)
	}
}

func TestCorruptPinsFileIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pinsFileName), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := openT(t, dir, 0)
	if len(c.Pins()) != 0 {
		t.Fatal("expected no pins")
	}
	if err := c.Pin("a", "user"); err != nil {
		t.Fatal(err)
	}
}

func TestTornPinsFileStartsEmpty(t *testing.T) {
	for _, body := range []string{"", "{", "{\"Version\":1,\"Pins\":{", "\x00\x00"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, pinsFileName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, pinsFileName+".torn"+tmpExt), []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := openT(t, dir, 0)
		if len(c.Pins()) != 0 {
			t.Fatalf("pins from %q: %v", body, c.Pins())
		}
		if _, err := os.Stat(filepath.Join(dir, pinsFileName+".torn"+tmpExt)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("torn temp %q: %v", body, err)
		}
	}
}

func TestIndexRebuiltOnRestart(t *testing.T) {
	dir := t.TempDir()
	c := openT(t, dir, 0)
	base := time.Now().Add(-time.Hour)
	var order []string // oldest first
	for i := range 6 {
		id := fmt.Sprintf("rel-%d/x", i%3)
		seg := i
		mustPut(t, c, id, seg, payload(id, seg, 0, 100+i))
		mt := base.Add(time.Duration(5-i) * time.Minute) // later puts get older mtimes
		if err := os.Chtimes(segPath(c, id, seg), mt, mt); err != nil {
			t.Fatal(err)
		}
		order = append([]string{fmt.Sprintf("%s#%d", id, seg)}, order...)
	}
	used, _ := c.Usage()
	rels := c.Releases()
	_ = c.Close()

	c2 := openT(t, dir, 0)
	used2, _ := c2.Usage()
	if used2 != used {
		t.Fatalf("used after restart=%d want %d", used2, used)
	}
	rels2 := c2.Releases()
	if len(rels2) != len(rels) {
		t.Fatalf("releases %d vs %d", len(rels2), len(rels))
	}
	for i := range 6 {
		id := fmt.Sprintf("rel-%d/x", i%3)
		got, ok := c2.Get(id, i)
		if !ok || !bytes.Equal(got, payload(id, i, 0, 100+i)) {
			t.Fatalf("Get(%q,%d) after restart ok=%v", id, i, ok)
		}
	}
	checkInvariants(t, c2)
	_ = c2.Close()

	// Reopen fresh (the Gets above touched mtimes, so restore them) with a
	// budget that forces eviction: oldest mtimes must go first.
	for i := range 6 {
		id := fmt.Sprintf("rel-%d/x", i%3)
		mt := base.Add(time.Duration(5-i) * time.Minute)
		if err := os.Chtimes(filepath.Join(dir, dataDirName, releaseDirRel(id), segName(i)), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	c3 := openT(t, dir, 100+101+102) // room for segments 0,1,2 (newest mtimes)
	for i := range 6 {
		id := fmt.Sprintf("rel-%d/x", i%3)
		if want := i < 3; c3.Has(id, i) != want {
			t.Fatalf("after budgeted reopen Has(%q,%d)=%v want %v (order %v)", id, i, !want, want, order)
		}
	}
	checkInvariants(t, c3)
}

func TestOpenCleansStaleFiles(t *testing.T) {
	dir := t.TempDir()
	c := openT(t, dir, 0)
	mustPut(t, c, "r", 0, payload("r", 0, 0, 100))
	mustPut(t, c, "r", 1, payload("r", 1, 0, 100))
	if err := c.Pin("r", "user"); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	rdir := filepath.Join(dir, dataDirName, releaseDirRel("r"))
	stale := []string{
		filepath.Join(rdir, "2.seg.123.tmp"),
		filepath.Join(rdir, "release.json.9.tmp"),
		filepath.Join(dir, "pins.json.77.tmp"),
		filepath.Join(dir, dataDirName, "x.tmp"),
	}
	for _, p := range stale {
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Torn segment shorter than the header.
	if err := os.WriteFile(filepath.Join(rdir, "5.seg"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Release dir without metadata.
	orphan := filepath.Join(dir, dataDirName, releaseDirRel("orphan"))
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "0.seg"), make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	// Release dir with metadata but no segments.
	empty := filepath.Join(dir, dataDirName, releaseDirRel("empty"))
	if err := writeReleaseMeta(empty, "empty"); err != nil {
		t.Fatal(err)
	}

	c2 := openT(t, dir, 0)
	for _, p := range append(stale, filepath.Join(rdir, "5.seg"), orphan, empty) {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s not cleaned: %v", p, err)
		}
	}
	if used, _ := c2.Usage(); used != 200 {
		t.Fatalf("used=%d", used)
	}
	if !c2.Has("r", 0) || !c2.Has("r", 1) || c2.Has("r", 5) {
		t.Fatal("wrong index after cleanup")
	}
	if !slices.Equal(c2.Pins()["r"], []string{"user"}) {
		t.Fatal("pins lost")
	}
	checkInvariants(t, c2)
}

func TestVanishedAndCorruptFiles(t *testing.T) {
	c := openT(t, t.TempDir(), 0)
	mustPut(t, c, "r", 0, payload("r", 0, 0, 100))
	mustPut(t, c, "r", 1, payload("r", 1, 0, 100))
	mustPut(t, c, "r", 2, payload("r", 2, 0, 100))

	if err := os.Remove(segPath(c, "r", 0)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("r", 0); ok {
		t.Fatal("vanished file served")
	}
	if c.Has("r", 0) {
		t.Fatal("index not fixed after vanished file")
	}

	// Same size, flipped payload byte: checksum must catch it.
	p := segPath(c, "r", 1)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("r", 1); ok {
		t.Fatal("corrupt file served")
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("corrupt file not removed")
	}

	// Truncated file.
	p = segPath(c, "r", 2)
	if err := os.Truncate(p, 50); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("r", 2); ok {
		t.Fatal("truncated file served")
	}
	if used, _ := c.Usage(); used != 0 {
		t.Fatalf("used=%d want 0", used)
	}
	checkInvariants(t, c)
}

func TestSetBudgetShrinkEvicts(t *testing.T) {
	c := openT(t, t.TempDir(), 0)
	for seg := range 10 {
		mustPut(t, c, "r", seg, payload("r", seg, 0, 100))
	}
	c.SetBudget(350)
	if used, budget := c.Usage(); used != 300 || budget != 350 {
		t.Fatalf("Usage=%d,%d", used, budget)
	}
	for seg := range 10 {
		if want := seg >= 7; c.Has("r", seg) != want {
			t.Fatalf("Has(%d)=%v want %v", seg, !want, want)
		}
	}
	if du, n := diskUsage(t, c); du != 300 || n != 3 {
		t.Fatalf("disk=%d,%d", du, n)
	}
	c.SetBudget(-1)
	if _, budget := c.Usage(); budget != 0 {
		t.Fatalf("negative budget not normalised: %d", budget)
	}
	checkInvariants(t, c)
}

func TestRemove(t *testing.T) {
	c := openT(t, t.TempDir(), 0)
	for seg := range 5 {
		mustPut(t, c, "a", seg, payload("a", seg, 0, 100))
	}
	mustPut(t, c, "b", 0, payload("b", 0, 0, 100))
	if err := c.Pin("a", "user"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if c.Has("a", 0) {
		t.Fatal("segment survived Remove")
	}
	if used, _ := c.Usage(); used != 100 {
		t.Fatalf("used=%d", used)
	}
	if du, n := diskUsage(t, c); du != 100 || n != 1 {
		t.Fatalf("disk=%d,%d", du, n)
	}
	if !slices.Equal(c.Pins()["a"], []string{"user"}) {
		t.Fatal("Remove dropped pins")
	}
	if err := c.Remove("unknown"); err != nil {
		t.Fatal(err)
	}
	mustPut(t, c, "a", 1, payload("a", 1, 1, 10)) // usable again
	if got, ok := c.Get("a", 1); !ok || !bytes.Equal(got, payload("a", 1, 1, 10)) {
		t.Fatal("put after Remove failed")
	}
	checkInvariants(t, c)
}

func TestReleaseDirRemovedExternally(t *testing.T) {
	c := openT(t, t.TempDir(), 0)
	mustPut(t, c, "r", 0, payload("r", 0, 0, 100))
	if err := os.RemoveAll(filepath.Join(c.dataDir, releaseDirRel("r"))); err != nil {
		t.Fatal(err)
	}
	mustPut(t, c, "r", 1, payload("r", 1, 0, 100))
	if _, ok := c.Get("r", 0); ok {
		t.Fatal("vanished served")
	}
	if _, ok := c.Get("r", 1); !ok {
		t.Fatal("put into recreated dir failed")
	}
}

func TestGetTouchRateLimited(t *testing.T) {
	c := openT(t, t.TempDir(), 0)
	c.touchEvery = time.Hour
	mustPut(t, c, "r", 0, payload("r", 0, 0, 10))
	p := segPath(c, "r", 0)
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	c.Get("r", 0)
	if info, _ := os.Stat(p); !info.ModTime().Equal(old) {
		t.Fatal("mtime touched within rate limit")
	}
	c.touchEvery = 0
	c.Get("r", 0)
	if info, _ := os.Stat(p); info.ModTime().Equal(old) {
		t.Fatal("mtime not touched")
	}
}

func TestConcurrentSameKey(t *testing.T) {
	c := openT(t, t.TempDir(), 0)
	var wg sync.WaitGroup
	for v := range 32 {
		wg.Go(func() {
			if err := c.Put("k", 0, payload("k", 0, v, 1000+v*37)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, ok := c.Get("k", 0)
	if !ok {
		t.Fatal("miss")
	}
	v := int(binary.LittleEndian.Uint64(got))
	if !bytes.Equal(got, payload("k", 0, v, 1000+v*37)) {
		t.Fatal("data does not match any single put")
	}
	if used, _ := c.Usage(); used != int64(len(got)) {
		t.Fatalf("used=%d len=%d", used, len(got))
	}
	if du, n := diskUsage(t, c); du != int64(len(got)) || n != 1 {
		t.Fatalf("disk=%d,%d", du, n)
	}
	checkInvariants(t, c)
}

func TestConcurrentStress(t *testing.T) {
	const (
		releases = 6
		segs     = 24
		maxSize  = 4096
		workers  = 16
		ops      = 600
	)
	budget := int64(40 * maxSize / 2)
	c := openT(t, t.TempDir(), budget)
	var protectOn atomic.Bool
	c.Protect(func() map[string]bool {
		if protectOn.Load() {
			return map[string]bool{"rel-5": true}
		}
		return nil
	})
	sizeOf := func(seg, v int) int { return 64 + (seg*131+v*977)%(maxSize-64) }
	var corrupt atomic.Int32
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 42))
			for range ops {
				id := fmt.Sprintf("rel-%d", rng.IntN(releases))
				seg := rng.IntN(segs)
				switch op := rng.IntN(100); {
				case op < 40:
					v := rng.IntN(1 << 20)
					if err := c.Put(id, seg, payload(id, seg, v, sizeOf(seg, v))); err != nil {
						t.Errorf("Put: %v", err)
					}
				case op < 80:
					if got, ok := c.Get(id, seg); ok {
						if len(got) < 8 {
							corrupt.Add(1)
							continue
						}
						v := int(binary.LittleEndian.Uint64(got))
						if !bytes.Equal(got, payload(id, seg, v, sizeOf(seg, v))) {
							corrupt.Add(1)
						}
					}
				case op < 88:
					c.Has(id, seg)
				case op < 92:
					if err := c.Pin(id, fmt.Sprintf("r%d", rng.IntN(2))); err != nil {
						t.Errorf("Pin: %v", err)
					}
				case op < 97:
					if err := c.Unpin(id, fmt.Sprintf("r%d", rng.IntN(2))); err != nil {
						t.Errorf("Unpin: %v", err)
					}
				case op < 98:
					protectOn.Store(!protectOn.Load())
				case op < 99:
					if err := c.Remove(id); err != nil {
						t.Errorf("Remove: %v", err)
					}
				default:
					c.Releases()
					c.Pins()
					c.Usage()
				}
			}
		})
	}
	wg.Wait()
	if n := corrupt.Load(); n != 0 {
		t.Fatalf("%d Gets returned data that was never Put for that key", n)
	}
	checkInvariants(t, c)
	if du, n := diskUsage(t, c); du != mustUsed(c) || n != c.segCountSafe() {
		t.Fatalf("disk=%d,%d index=%d,%d", du, n, mustUsed(c), c.segCountSafe())
	}

	// Lift all protection: usage must come down to the budget.
	for id, reasons := range c.Pins() {
		for _, r := range reasons {
			if err := c.Unpin(id, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.Protect(nil)
	if used, b := c.Usage(); used > b {
		t.Fatalf("used=%d > budget=%d with nothing protected", used, b)
	}
	checkInvariants(t, c)
	if du, n := diskUsage(t, c); du != mustUsed(c) || n != c.segCountSafe() {
		t.Fatalf("disk=%d,%d index=%d,%d", du, n, mustUsed(c), c.segCountSafe())
	}
	// Every indexed segment is still served intact.
	for r := range releases {
		id := fmt.Sprintf("rel-%d", r)
		for seg := range segs {
			if !c.Has(id, seg) {
				continue
			}
			got, ok := c.Get(id, seg)
			if !ok {
				t.Fatalf("indexed %s/%d missing on disk", id, seg)
			}
			v := int(binary.LittleEndian.Uint64(got))
			if !bytes.Equal(got, payload(id, seg, v, sizeOf(seg, v))) {
				t.Fatalf("%s/%d corrupt", id, seg)
			}
		}
	}
}

func mustUsed(c *Cache) int64 { u, _ := c.Usage(); return u }

func BenchmarkPut(b *testing.B) {
	c := openT(b, b.TempDir(), 64<<20)
	data := payload("bench", 0, 0, 700<<10)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	i := 0
	for b.Loop() {
		if err := c.Put("bench", i%256, data); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkGet(b *testing.B) {
	c := openT(b, b.TempDir(), 0)
	c.touchEvery = defaultTouchEvery
	data := payload("bench", 0, 0, 700<<10)
	for seg := range 32 {
		mustPut(b, c, "bench", seg, data)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	i := 0
	for b.Loop() {
		if _, ok := c.Get("bench", i%32); !ok {
			b.Fatal("miss")
		}
		i++
	}
}

func BenchmarkHasParallel(b *testing.B) {
	c := openT(b, b.TempDir(), 0)
	for seg := range 64 {
		mustPut(b, c, "bench", seg, []byte("x"))
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Has("bench", i%64)
			i++
		}
	})
}

func TestReleaseBytes(t *testing.T) {
	const sz = 100
	c := openT(t, t.TempDir(), 0)
	for seg := range 3 {
		mustPut(t, c, "r", seg, payload("r", seg, 0, sz))
	}
	if got := c.ReleaseBytes("r"); got != 3*sz {
		t.Fatalf("ReleaseBytes = %d, want %d", got, 3*sz)
	}
	if got := c.ReleaseBytes("missing"); got != 0 {
		t.Fatalf("ReleaseBytes(missing) = %d", got)
	}
}
