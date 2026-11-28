package blobcache

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/segcache"
)

func sharedFileStore(t *testing.T, dir string, limit int64) *Cache {
	t.Helper()
	c, err := Open(dir, Options{Shared: true, SweepPeriod: -1, PoolCapacity: func() int64 { return limit }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestSharedWholeFilesAcrossNodesAndRestart(t *testing.T) {
	dir := t.TempDir()
	a, b := sharedFileStore(t, dir, 1<<30), sharedFileStore(t, dir, 1<<30)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		key := fmt.Sprintf("disc%d.iso", i)
		node := []*Cache{a, b}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			ns := node.Namespace("games", Quota{})
			if _, err := ns.Put(ctx, key, strings.NewReader(key), int64(len(key))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, c := range []*Cache{a, b} {
		if n := len(c.Files()); n != 12 {
			t.Fatalf("%d published discs, want 12", n)
		}
	}
	nsA := a.Namespace("games", Quota{})
	ref, ok := nsA.LookupKey("disc5.iso")
	if !ok {
		t.Fatal("missing remote publication")
	}
	if err := nsA.AddAliases(ref, []string{"playable"}); err != nil {
		t.Fatal(err)
	}
	open, err := b.Namespace("games", Quota{}).OpenByKey("playable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.EvictFile("games", ref); !errors.Is(err, ErrFileInUse) {
		t.Fatalf("eviction of remote open = %v", err)
	}
	if freed, err := a.ClearFiles(); !errors.Is(err, ErrFileInUse) || freed == 0 {
		t.Fatalf("clear = %d %v", freed, err)
	}
	if len(b.Files()) != 1 {
		t.Fatal("clear did not retain exactly the leased file")
	}
	open.Close()
	a.Close()
	b.Close()
	c := sharedFileStore(t, dir, 1<<30)
	src, err := c.Namespace("games", Quota{}).OpenByKey("playable")
	if err != nil {
		t.Fatal(err)
	}
	buf, err := io.ReadAll(src)
	if err != nil || string(buf) != "disc5.iso" {
		t.Fatalf("offline read %q %v", buf, err)
	}
	src.Close()
	if size, err := c.EvictFile("games", ref); err != nil || size != 9 {
		t.Fatalf("evict = %d %v", size, err)
	}
	if _, err := c.EvictFile("games", ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evict missing = %v", err)
	}
}

func TestSharedReservationsAndStagedAccounting(t *testing.T) {
	dir := t.TempDir()
	a, b := sharedFileStore(t, dir, 25), sharedFileStore(t, dir, 25)
	pa, err := a.Namespace("games", Quota{}).OpenPartial(context.Background(), "game")
	if err != nil {
		t.Fatal(err)
	}
	defer pa.Close()
	if err := pa.Reserve(20); err != nil {
		t.Fatal(err)
	}
	if _, err := pa.Write([]byte("seven!!")); err != nil {
		t.Fatal(err)
	}
	pb, err := b.Namespace("other", Quota{}).OpenPartial(context.Background(), "another")
	if err != nil {
		t.Fatal(err)
	}
	defer pb.Close()
	if err := pb.Reserve(6); !errors.Is(err, ErrQuotaFull) {
		t.Fatalf("oversubscription = %v", err)
	}
	// A waiting writer on node B must not hide node A's outstanding capacity.
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	wait := make(chan error, 1)
	go func() {
		p, err := b.Namespace("games", Quota{}).OpenPartial(waitCtx, "game")
		if p != nil {
			p.Close()
		}
		wait <- err
	}()
	used, r, err := b.FileUsage()
	if err != nil || used != 7 || r != 13 {
		t.Fatalf("usage %d+%d %v", used, r, err)
	}
	cancel()
	if err := <-wait; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected wait %v", err)
	}
	if err := pb.Reserve(5); err != nil {
		t.Fatal(err)
	}
	pa.Close()
	used, r, err = b.FileUsage()
	if err != nil || used != 7 || r != 5 {
		t.Fatalf("released reservation %d+%d %v", used, r, err)
	}
	if _, ok := a.Namespace("games", Quota{}).LookupKey("game"); ok {
		t.Fatal("exposed incomplete transfer")
	}
	pb.Close()
	if freed, err := b.ClearFiles(); err != nil || freed != 7 {
		t.Fatalf("clear staged = %d %v", freed, err)
	}
}

func TestFilePoolQuotaInheritanceAndDigest(t *testing.T) {
	dir := t.TempDir()
	a := sharedFileStore(t, dir, 128<<30)
	ns := a.Namespace("games", Quota{})
	if ns.Quota().MaxBytes != 0 {
		t.Fatal("library should inherit media budget")
	}
	p, err := ns.OpenPartial(context.Background(), "large.iso")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve(32 << 30); err != nil {
		t.Fatalf("large disc admission: %v", err)
	}
	p.Close()
	ns = a.Namespace("games", Quota{MaxBytes: 32})
	if err := ns.SetOwnerQuota(Quota{MaxBytes: 8}); err != nil {
		t.Fatal(err)
	}
	b := sharedFileStore(t, dir, 128<<30)
	ns = b.Namespace("games", Quota{MaxBytes: 64})
	if ns.Quota().MaxBytes != 8 {
		t.Fatal("owner quota lost after bind")
	}
	if _, err := ns.Put(context.Background(), "bad", bytes.NewReader(make([]byte, 9)), 9); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit ignored: %v", err)
	}
	if err := ns.SetOwnerQuota(Quota{}); err != nil || ns.Quota().MaxBytes != 64 {
		t.Fatalf("failed to restore manifest quota: %v", err)
	}
	if _, err := ns.PutWithExpectedSHA256(context.Background(), "bad", strings.NewReader("no"), 2, strings.Repeat("0", 64), ""); err == nil {
		t.Fatal("accepted bad checksum")
	}
	if u, r, err := a.FileUsage(); u != 0 || r != 0 || err != nil {
		t.Fatalf("failed upload left %d+%d: %v", u, r, err)
	}
}

func TestSharedIdleAdmissionUsesCompactCounter(t *testing.T) {
	dir := t.TempDir()
	a := sharedFileStore(t, dir, 1000)
	if _, err := a.Namespace("games", Quota{}).Put(context.Background(), "disc", strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}
	b := sharedFileStore(t, dir, 1000)
	for i := 0; i < 2; i++ {
		u, r, e := b.FileUsage()
		if u != 3 || r != 0 || e != nil {
			t.Fatalf("usage = %d+%d %v", u, r, e)
		}
	}
	// A fresh namespace containing a deliberately unreadable index makes a
	// catalogue traversal observable. Idle segment puts need no such traversal.
	newDir := filepath.Join(dir, "sentinel")
	if err := os.Mkdir(newDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "index.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	// Read-only LookupKey and Quota checks must neither block on the pool
	// allocation lock nor invalidate the .idle-usage snapshot.
	lockFile, err := os.OpenFile(filepath.Join(dir, ".allocation.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if ok, err := tryFileLock(lockFile, true); err != nil || !ok {
		t.Fatalf("tryFileLock = (%v, %v)", ok, err)
	}
	nsB := b.Namespace("games", Quota{MaxBytes: 500})
	if q := nsB.Quota(); q.MaxBytes != 500 {
		t.Fatalf("Quota().MaxBytes = %d, want 500", q.MaxBytes)
	}
	if _, ok := nsB.LookupKey("disc"); !ok {
		t.Fatal("LookupKey missed published disc")
	}
	lockFile.Close()
	if _, err := os.Stat(filepath.Join(dir, ".idle-usage")); err != nil {
		t.Fatalf(".idle-usage removed by read-only LookupKey/Quota: %v", err)
	}
	if err := b.WithAllocation(func(a, r int64, _ segcache.FileCandidates) error {
		if a != 3 || r != 0 {
			t.Fatalf("counter = %d+%d", a, r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseReleasedOnProcessExit(t *testing.T) {
	if dir := os.Getenv("ETHER_TEST_CRASH_CACHE"); dir != "" {
		c := sharedFileStore(t, dir, 100)
		open, err := c.Namespace("games", Quota{}).OpenByKey("leased-disc")
		if err != nil {
			t.Fatal(err)
		}
		defer open.Close()
		p, err := c.Namespace("games", Quota{}).OpenPartial(context.Background(), "download")
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Reserve(90); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		if err := p.Sync(); err != nil {
			t.Fatal(err)
		}
		fmt.Println("leased")
		bufio.NewReader(os.Stdin).ReadByte()
		os.Exit(0) // no Close; the OS releases the cross-node file lease
	}
	dir := t.TempDir()
	c := sharedFileStore(t, dir, 100)
	ref, err := c.Namespace("games", Quota{}).Put(context.Background(), "leased-disc", strings.NewReader("hello"), 5)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLeaseReleasedOnProcessExit$")
	cmd.Env = append(os.Environ(), "ETHER_TEST_CRASH_CACHE="+dir)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		if cmd.ProcessState == nil {
			cmd.Wait()
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "leased" {
		t.Fatalf("child startup %q %v", line, err)
	}
	if u, r, e := c.FileUsage(); u != 9 || r != 86 || e != nil {
		t.Fatalf("other process %d+%d %v", u, r, e)
	}
	if n := c.ProtectedBytes(); n != 9 {
		t.Fatalf("protected %d", n)
	}
	if _, err := c.EvictFile("games", ref); !errors.Is(err, ErrFileInUse) {
		t.Fatalf("remote read could be evicted: %v", err)
	}
	in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if u, r, e := c.FileUsage(); u != 9 || r != 0 || e != nil {
		t.Fatalf("after exit %d+%d %v", u, r, e)
	}
	if n, err := c.EvictFile("games", ref); err != nil || n != 5 {
		t.Fatalf("after process exit %d %v", n, err)
	}
	p, err := c.Namespace("games", Quota{}).OpenPartial(context.Background(), "download")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Size() != 4 {
		t.Fatal("checkpoint lost")
	}
}

func BenchmarkSharedIdleAdmission(b *testing.B) {
	c, err := Open(b.TempDir(), Options{Shared: true, SweepPeriod: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 100; i++ {
		_, err = c.Namespace("games", Quota{}).Put(context.Background(), strconv.Itoa(i), strings.NewReader(strconv.Itoa(i)), -1)
		if err != nil {
			b.Fatal(err)
		}
	}
	c.FileUsage()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.WithAllocation(func(int64, int64, segcache.FileCandidates) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSharedPositionedTransferChargesHolesToDiskFloor(t *testing.T) {
	dir := t.TempDir()
	a, b := sharedFileStore(t, dir, 100), sharedFileStore(t, dir, 100)
	p, err := a.Namespace("games", Quota{}).OpenPartial(context.Background(), "disc")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Reset(PartialMeta{ContentLength: 100, RangeProgress: make([]int64, 2)}); err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve(100); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteRange(1, []byte("end")); err != nil {
		t.Fatal(err)
	}
	if err := p.Sync(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Cache{a, b} {
		u, r, e := c.FileUsage()
		if u != 3 || r != 97 || e != nil {
			t.Fatalf("positioned usage = %d+%d %v", u, r, e)
		}
		if got := c.ProtectedBytes(); got != 3 {
			t.Fatalf("protected %d", got)
		}
	}
}

func TestSharedVolumeGuardRejectsMissingMount(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "nas", "plugin-media")
	gone := errors.New("volume disconnected")
	online := false
	opts := Options{Shared: true, SweepPeriod: -1, CheckVolume: func() error {
		if !online {
			return gone
		}
		return nil
	}}
	if _, err := Open(root, opts); !errors.Is(err, gone) {
		t.Fatalf("startup %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("created a directory under missing mount")
	}
	online = true
	c, err := Open(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ns := c.Namespace("games", Quota{})
	p, err := ns.OpenPartial(context.Background(), "disc")
	if err != nil {
		t.Fatal(err)
	}
	online = false
	if err := p.Reserve(20); !errors.Is(err, gone) {
		t.Fatalf("reserve %v", err)
	}
	if _, r, err := c.FileUsage(); !errors.Is(err, gone) || r != 0 {
		t.Fatalf("offline usage %d %v", r, err)
	}
	if err := p.Reset(PartialMeta{}); !errors.Is(err, gone) {
		t.Fatalf("reset %v", err)
	}
	p.Close()
}

func TestRecoverInterruptedFilePublication(t *testing.T) {
	dir := t.TempDir()
	c := sharedFileStore(t, dir, 100)
	ns := c.Namespace("games", Quota{})
	ref, err := ns.Put(context.Background(), "existing.iso", strings.NewReader("kept"), 4)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := ref.Hash()
	// A crash after replacing the index must retain the file and its aliases.
	if err := os.WriteFile(filepath.Join(ns.dir, ".publishing"), []byte(ref), 0600); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = sharedFileStore(t, dir, 100)
	ns = c.Namespace("games", Quota{})
	if got, ok := ns.LookupKey("existing.iso"); !ok || got != ref {
		t.Fatal("lost published file")
	}
	// A crash before replacing the index leaves no readable file and no
	// permanent, unaccounted bytes.
	orphan := strings.Repeat("a", 64)
	if orphan == h {
		t.Fatal("unexpected test digest")
	}
	path := ns.blobPath(orphan)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ns.dir, ".publishing"), []byte("blob:"+orphan), 0600); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = sharedFileStore(t, dir, 100)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("orphan survived: %v", err)
	}
	if u, r, err := c.FileUsage(); u != 4 || r != 0 || err != nil {
		t.Fatalf("recovered usage %d+%d %v", u, r, err)
	}
}

// A lock-free LookupKey must not reload the index over a local mutation that
// has not been saved yet (a Put between updating memory and flushIndex).
func TestSharedLookupKeepsUnsavedLocalIndex(t *testing.T) {
	dir := t.TempDir()
	a, b := sharedFileStore(t, dir, 1<<30), sharedFileStore(t, dir, 1<<30)
	ctx := context.Background()
	nsA := a.Namespace("games", Quota{MaxBytes: 1 << 20})
	ref, err := nsA.Put(ctx, "one", strings.NewReader("first"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Namespace("games", Quota{MaxBytes: 1 << 20}).Put(ctx, "two", strings.NewReader("second"), 6); err != nil {
		t.Fatal(err)
	}
	nsA.mu.Lock()
	nsA.keys["pending"] = strings.TrimPrefix(string(ref), "blob:")
	nsA.dirty = true
	nsA.mu.Unlock()
	nsA.LookupKey("two")
	nsA.mu.Lock()
	_, kept := nsA.keys["pending"]
	dirty := nsA.dirty
	nsA.mu.Unlock()
	if !kept || !dirty {
		t.Fatalf("unsaved local index entry dropped (kept=%v dirty=%v)", kept, dirty)
	}
}
