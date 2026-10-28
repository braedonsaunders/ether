package resolver

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPruneExpiresBadMarksAndStaleRows(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	if err := svc.MarkBad(ctx, "old", "broken"); err != nil {
		t.Fatal(err)
	}
	svc.setAvailability(ctx, 1, true, time.Hour)
	if _, err := db.ExecContext(ctx, `INSERT INTO resolver_verified VALUES('stale', ?)`, clock.now().Unix()); err != nil {
		t.Fatal(err)
	}

	clock.add(badRetention - time.Hour)
	if err := svc.MarkBad(ctx, "recent", "broken"); err != nil {
		t.Fatal(err)
	}
	n, err := svc.Prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Expired availability and the stale health pass go; bad marks stay.
	if n != 2 {
		t.Fatalf("first prune removed %d rows, want 2", n)
	}
	if !svc.isBad("old") || !svc.isBad("recent") {
		t.Fatal("bad marks inside retention must survive")
	}

	clock.add(2 * time.Hour)
	if svc.isBad("old") {
		t.Fatal("bad mark past retention still honoured")
	}
	if n, err = svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("second prune removed %d rows, want 1 (the old bad mark)", n)
	}
	var ids []string
	rows, err := db.QueryContext(ctx, `SELECT release_id FROM resolver_bad`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != "recent" {
		t.Fatalf("resolver_bad = %v, want [recent]", ids)
	}
	svc.mu.RLock()
	nb, na := len(svc.bad), len(svc.avail)
	svc.mu.RUnlock()
	if nb != 1 || na != 0 {
		t.Fatalf("in-memory bad=%d avail=%d, want 1 and 0", nb, na)
	}

	// A restart only loads marks inside retention.
	clock.add(badRetention)
	svc2 := New(idx, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	if svc2.isBad("recent") || len(svc2.bad) != 0 {
		t.Fatal("expired marks loaded at startup")
	}
}

func TestPruneKeepsRecentUnavailableForRecheckPeek(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	t0 := clock.now()
	svc := New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	svc.setAvailability(ctx, 42, false, time.Minute)
	svc.setAvailability(ctx, 43, true, time.Minute)

	// Expired (1m TTL) but checked 2m ago, inside unavailableRecheck:
	// Prune must keep the unavailable entry for the peek, in memory and DB.
	clock.add(2 * time.Minute)
	if _, err := svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.getAvailability(ctx, 42); ok {
		t.Fatal("preserved entry must stay expired for getAvailability")
	}
	if checked, ok := svc.lastUnavailableChecked(ctx, 42); !ok || !checked.Equal(t0) {
		t.Fatalf("peek after prune (memory) checked=%v ok=%v want %v,true", checked, ok, t0)
	}
	freshSvc := New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	if checked, ok := freshSvc.lastUnavailableChecked(ctx, 42); !ok || !checked.Equal(t0) {
		t.Fatalf("peek after prune (DB) checked=%v ok=%v want %v,true", checked, ok, t0)
	}
	// The available=true entry is still pruned normally.
	var availRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resolver_availability WHERE rating_key=43`).Scan(&availRows); err != nil {
		t.Fatal(err)
	}
	if availRows != 0 {
		t.Fatalf("available=true rows after prune = %d, want 0", availRows)
	}

	// Checked now older than unavailableRecheck: Prune drops it and the
	// peek goes away in memory and DB.
	clock.add(4 * time.Minute) // 6m since the check
	if _, err := svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.lastUnavailableChecked(ctx, 42); ok {
		t.Fatal("peek still ok after prune past recheck window (memory)")
	}
	freshSvc = New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	if _, ok := freshSvc.lastUnavailableChecked(ctx, 42); ok {
		t.Fatal("peek still ok after prune past recheck window (DB)")
	}
}

func TestRemarkingBadRefreshesRetention(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	svc := New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	_ = svc.MarkBad(ctx, "r", "a")
	clock.add(badRetention - time.Hour)
	_ = svc.MarkBad(ctx, "r", "b")
	clock.add(2 * time.Hour)
	if _, err := svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if !svc.isBad("r") {
		t.Fatal("re-marked release lost its mark")
	}
	var n int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resolver_bad`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestBoundMapEvictsExpiredThenArbitrary(t *testing.T) {
	m := map[int]int{}
	for i := 0; i < 10; i++ {
		m[i] = i
	}
	boundMap(m, 20, func(v int) bool { return true })
	if len(m) != 10 {
		t.Fatal("below cap must not evict")
	}
	boundMap(m, 10, func(v int) bool { return v < 4 })
	if len(m) != 6 {
		t.Fatalf("len = %d, want 6 after dropping expired", len(m))
	}
	for i := 0; i < 4; i++ {
		if _, ok := m[i]; ok {
			t.Fatal("expired entry survived")
		}
	}
	for i := 10; i < 14; i++ {
		m[i] = i
	}
	boundMap(m, 10, func(int) bool { return false })
	if len(m) != 7 {
		t.Fatalf("len = %d, want 7 (3/4 of cap)", len(m))
	}
}

func TestSearchCacheIsBounded(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeIndexer{}, fakeHealth{}, nil, testCfg(), Options{})
	for i := 0; i < maxSearchCache+50; i++ {
		if _, err := svc.search(ctx, core.SearchRequest{Title: "t", Year: i}); err != nil {
			t.Fatal(err)
		}
	}
	svc.mu.RLock()
	n := len(svc.searchCache)
	svc.mu.RUnlock()
	if n > maxSearchCache {
		t.Fatalf("search cache holds %d entries, cap %d", n, maxSearchCache)
	}
}

func TestOwnerBlocksNeverExpire(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	svc := New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	if _, err := svc.Block(ctx, BlockRequest{ReleaseID: "owner"}, "bad audio"); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkBad(ctx, "auto", "missing articles"); err != nil {
		t.Fatal(err)
	}
	// An automatic mark on an owner block must not make it expire.
	if err := svc.MarkBad(ctx, "owner", "missing articles"); err != nil {
		t.Fatal(err)
	}

	clock.add(2 * badRetention)
	if !svc.isBad("owner") {
		t.Fatal("owner block expired in memory")
	}
	if svc.isBad("auto") {
		t.Fatal("automatic mark past retention still honoured")
	}
	if n, err := svc.Prune(ctx); err != nil || n != 1 {
		t.Fatalf("prune removed %d rows (err %v), want 1 (the automatic mark)", n, err)
	}
	var reason string
	var manual int
	if err := db.QueryRowContext(ctx, `SELECT reason, manual FROM resolver_bad WHERE release_id='owner'`).Scan(&reason, &manual); err != nil {
		t.Fatalf("owner block pruned: %v", err)
	}
	if reason != "bad audio" || manual != 1 {
		t.Fatalf("owner block = %q manual=%d, want the owner's reason kept", reason, manual)
	}

	// A restart loads owner blocks however old they are.
	svc2 := New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	if !svc2.isBad("owner") {
		t.Fatal("owner block not loaded at startup")
	}
	if r, ok := svc2.badReason("owner"); !ok || r != "bad audio" {
		t.Fatalf("badReason = %q, %v", r, ok)
	}
}
