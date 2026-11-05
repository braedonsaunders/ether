package livetv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/braedonsaunders/ether/internal/store"
)

// Use the same 701 channels and 12s/20s programme items as the bulk atomicity
// fixture. Short media must not make a metadata edit re-materialize the guide.
func TestGuideSnapshotBulkMetadataMemory(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	// Exercise the real owner-save and publication methods with an idle
	// maintenance scheduler, making heap sampling independent of ticker timing.
	s := &Service{o: Options{DB: db, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, ctx: ctx}
	ids := make([]string, 701)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		c := testChannel()
		c.ID = fmt.Sprintf("channel-%04d", i)
		ids[i] = c.ID
		c.StreamURL = "https://provider.invalid/private"
		c.Private = true
		raw, e := json.Marshal(c)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO livetv_channels(id,data) VALUES(?,?)", c.ID, raw); e != nil {
			t.Fatal(e)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
			if peak.CompareAndSwap(old, m.HeapAlloc) {
				break
			}
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()
	started := time.Now()
	if err = s.refreshVirtualGuide(ctx); err != nil {
		t.Fatal(err)
	}
	sample()
	s.virtualGuide.mu.RLock()
	originals := make(map[string]*compactProgrammes, len(ids))
	rows := 0
	uniqueRows := map[*compactProgrammeRow]int{}
	for _, id := range ids {
		entry := s.virtualGuide.entries[id]
		if entry == nil || entry.raw == nil {
			s.virtualGuide.mu.RUnlock()
			t.Fatalf("missing snapshot for %s", id)
		}
		originals[id] = entry.raw
		rows += len(entry.raw.rows)
		if len(entry.raw.rows) > 0 {
			uniqueRows[&entry.raw.rows[0]] = len(entry.raw.rows)
		}
	}
	s.virtualGuide.mu.RUnlock()
	initial := time.Since(started)
	first, err := s.Channel(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	windowFrom := now
	windowBefore, _ := json.Marshal(s.virtualProgrammes(first, windowFrom, windowFrom.Add(time.Minute), "raw"))
	// Metadata reuse must also outlive the former thirty-second rebuild TTL.
	now = now.Add(time.Minute)
	favorite := true
	updateStart := time.Now()
	n, err := s.UpdateChannels(ctx, ChannelBatch{IDs: ids, Favorite: &favorite})
	if err != nil || n != len(ids) {
		t.Fatalf("bulk favorite: n=%d err=%v", n, err)
	}
	updateElapsed := time.Since(updateStart)
	sample()
	s.virtualGuide.mu.RLock()
	for _, id := range ids {
		entry := s.virtualGuide.entries[id]
		if entry == nil || entry.raw != originals[id] {
			t.Errorf("metadata update rebuilt schedule for %s", id)
			break
		}
		if !entry.channel.Favorite {
			t.Errorf("favorite metadata stale for %s", id)
			break
		}
	}
	s.virtualGuide.mu.RUnlock()
	// The same invariant covers the record editor's other metadata fields.
	c, err := s.Channel(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	c.Name = "Updated station"
	c.Number = "99"
	c.Logo = "/new-logo"
	c.Enabled = false
	c.Private = false
	c.Group = "Updated group"
	c.CustomGroup = true
	if _, err = s.SaveChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	sample()
	s.virtualGuide.mu.RLock()
	entry := s.virtualGuide.entries[c.ID]
	retained := entry != nil && entry.raw == originals[c.ID]
	s.virtualGuide.mu.RUnlock()
	if !retained {
		t.Fatal("record metadata update rebuilt its schedule")
	}
	windowAfter, _ := json.Marshal(s.virtualProgrammes(c, windowFrom, windowFrom.Add(time.Minute), "raw"))
	if string(windowBefore) != string(windowAfter) {
		t.Fatal("metadata update changed programme output")
	}
	if size := unsafe.Sizeof(compactProgrammeRow{}); size > 32 {
		t.Fatalf("compact programme row grew to %d bytes", size)
	}
	storedRows := 0
	for _, n := range uniqueRows {
		storedRows += n
	}
	const limit = uint64(512 << 20)
	delta := peak.Load() - before.HeapAlloc
	t.Logf("channels=%d logical_rows=%d full Programme=%d bytes compact row=%d bytes initial=%s metadata_update=%s baseline_heap=%d peak_heap=%d incremental_peak=%d bound=%d", len(ids), rows, unsafe.Sizeof(Programme{}), unsafe.Sizeof(compactProgrammeRow{}), initial, updateElapsed, before.HeapAlloc, peak.Load(), delta, limit)
	t.Logf("unique_row_tables=%d stored_rows=%d row_storage_bytes=%d", len(uniqueRows), storedRows, uintptr(storedRows)*unsafe.Sizeof(compactProgrammeRow{}))
	if delta > limit {
		t.Fatalf("701-channel warm and metadata update exceeded 512 MiB incremental peak heap: %d bytes", delta)
	}
	if updateElapsed > 10*time.Second {
		t.Fatalf("metadata-only bulk update took %s; budget 10s", updateElapsed)
	}
	runtime.KeepAlive(s)
}
