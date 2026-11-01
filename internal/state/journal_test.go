package state

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestJournalCommitsWithWatchState(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db, Options{})
	key := core.MovieKey(550)
	if err := s.Scrobble(ctx, 7, key); err != nil {
		t.Fatal(err)
	}
	row, ok, err := s.ClaimSync(ctx, "node-a", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if row.UserID != 7 || row.Key != key || row.Kind != SyncComplete || row.Aspect != SyncWatch || row.Bulk {
		t.Fatalf("row %+v", row)
	}
	ws, err := s.Get(ctx, 7, key)
	if err != nil || ws.ViewCount != 1 {
		t.Fatalf("watch %+v %v", ws, err)
	}
}

func TestJournalRollbackDropsWatchAndIntent(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db, Options{})
	s.SetJournalFault(func() error { return errors.New("journal full") })
	if err := s.Scrobble(ctx, 7, core.MovieKey(550)); err == nil {
		t.Fatal("scrobble succeeded despite journal failure")
	}
	ws, err := s.Get(ctx, 7, core.MovieKey(550))
	if err != nil || ws.ViewCount != 0 {
		t.Fatalf("watch survived rollback: %+v %v", ws, err)
	}
	_, ok, err := s.ClaimSync(ctx, "node-a", time.Now(), time.Minute)
	if err != nil || ok {
		t.Fatalf("intent survived rollback: %v %v", ok, err)
	}
}

func TestRepeatedProgressJournalsOneCompletion(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db, Options{})
	key := core.EpisodeKey(1396, 1, 3)
	dur := 100 * time.Second
	for _, off := range []time.Duration{95 * time.Second, 99 * time.Second, 100 * time.Second} {
		if err := s.Progress(ctx, 3, key, off, dur, "playing"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM state_sync_journal WHERE kind=?`, SyncComplete).Scan(&n); err != nil || n != 1 {
		t.Fatalf("completion rows %d %v", n, err)
	}
	row, ok, err := s.ClaimSync(ctx, "n", time.Now(), time.Minute)
	if err != nil || !ok || row.Kind != SyncComplete || row.Bulk {
		t.Fatalf("first %+v %v %v", row, ok, err)
	}
}

func TestNewerIntentSupersedesClaimedRetry(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db, Options{})
	key := core.MovieKey(1)
	if err := s.Scrobble(ctx, 4, key); err != nil {
		t.Fatal(err)
	}
	old, ok, err := s.ClaimSync(ctx, "n", time.Now(), time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.Unscrobble(ctx, 4, key); err != nil {
		t.Fatal(err)
	}
	live, err := s.SyncCurrent(ctx, old.Seq, "n")
	if err != nil || live {
		t.Fatalf("old intent still current: %v %v", live, err)
	}
	next, ok, err := s.ClaimSync(ctx, "n", time.Now(), time.Minute)
	if err != nil || !ok || next.Kind != SyncUnplayed || next.Seq == old.Seq {
		t.Fatalf("newer %+v %v %v", next, ok, err)
	}
}

func TestImportDoesNotJournal(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db, Options{})
	if err := s.ImportTransaction(ctx, func(*sql.Tx) (ImportBatch, error) {
		return ImportBatch{Records: []ImportRecord{{
			UserID: 2, Key: core.MovieKey(9), Views: 1, ViewedAt: time.Unix(100, 0),
		}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimSync(ctx, "n", time.Now(), time.Minute); err != nil || ok {
		t.Fatalf("import enqueued an upload: %v %v", ok, err)
	}
}

func TestApplyRemoteSkipsNewerLocal(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(db, Options{Now: func() time.Time { return now }})
	key := core.MovieKey(3)
	if err := s.Scrobble(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	watched := false
	err := s.ApplyRemote(ctx, 1, key, &watched, now.Add(-time.Hour), nil, time.Time{})
	if !errors.Is(err, ErrLocalNewer) {
		t.Fatalf("err %v", err)
	}
	ws, err := s.Get(ctx, 1, key)
	if err != nil || ws.ViewCount != 1 {
		t.Fatalf("local reversed: %+v %v", ws, err)
	}
	// The local scrobble is still the only live intent.
	pending, _, err := s.PendingSyncCount(ctx, 1)
	if err != nil || pending != 1 {
		t.Fatalf("pending %d %v", pending, err)
	}
}

func TestJournalGateSkipsDisconnectedViewers(t *testing.T) {
	ctx := context.Background()
	s := New(store.OpenTest(t), Options{})
	s.SetJournalGate(func(context.Context, *sql.Tx, int64) (int64, bool, error) { return 0, false, nil })
	if err := s.Scrobble(ctx, 4, core.MovieKey(1)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimSync(ctx, "n", time.Now(), time.Minute); err != nil || ok {
		t.Fatalf("disconnected viewer journaled %v %v", ok, err)
	}
	s.SetJournalGate(func(context.Context, *sql.Tx, int64) (int64, bool, error) { return 1, true, nil })
	if err := s.Scrobble(ctx, 4, core.MovieKey(2)); err != nil {
		t.Fatal(err)
	}
	row, ok, err := s.ClaimSync(ctx, "n", time.Now(), time.Minute)
	if err != nil || !ok || row.Key != core.MovieKey(2) {
		t.Fatalf("enabled viewer %+v %v %v", row, ok, err)
	}
}

func TestCompactKeepsLiveIntent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	db := store.OpenTest(t)
	s := New(db, Options{Now: func() time.Time { return now }})
	if err := s.Scrobble(ctx, 3, core.MovieKey(9)); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-30 * 24 * time.Hour).UnixNano()
	if _, err := db.Exec(`INSERT INTO state_sync_journal(user_id, rating_key, aspect, kind, occurred_at, dead) VALUES(?,?,?,?,?,1)`,
		3, int64(core.MovieKey(8)), "watch", "complete", old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state_sync_lock(user_id, rating_key, aspect, holder, until_at) VALUES(?,?,?,'',0)`,
		3, int64(core.MovieKey(8)), "watch"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state_sync_lock(user_id, rating_key, aspect, holder, until_at) VALUES(?,?,?,?,?)`,
		3, int64(core.MovieKey(9)), "watch", "node", now.Add(time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := s.CompactSync(ctx, now); err != nil {
		t.Fatal(err)
	}
	row, ok, err := s.ClaimSync(ctx, "n", now, time.Minute)
	if err != nil || !ok || row.Key != core.MovieKey(9) {
		t.Fatalf("live intent %+v %v %v", row, ok, err)
	}
	var dead int
	if err := db.QueryRow(`SELECT COUNT(*) FROM state_sync_journal WHERE dead=1`).Scan(&dead); err != nil || dead != 0 {
		t.Fatalf("terminal rows %d %v", dead, err)
	}
	var locks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM state_sync_lock`).Scan(&locks); err != nil || locks != 1 {
		t.Fatalf("locks %d %v", locks, err)
	}
}

func TestApplyRemoteIndependentClocks(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := New(store.OpenTest(t), Options{Now: func() time.Time { return now }})
	key := core.MovieKey(8)
	if err := s.Rate(ctx, 1, key, 10); err != nil {
		t.Fatal(err)
	}
	watched := true
	older := 2.0
	if err := s.ApplyRemote(ctx, 1, key, &watched, now.Add(time.Hour), &older, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	ws, err := s.Get(ctx, 1, key)
	if err != nil || ws.ViewCount != 1 || ws.UserRating != 10 {
		t.Fatalf("newer watch applied an older rating: %+v %v", ws, err)
	}
	key = core.MovieKey(9)
	if err := s.Scrobble(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRemote(ctx, 1, key, nil, time.Time{}, &older, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	ws, err = s.Get(ctx, 1, key)
	if err != nil || ws.ViewCount != 1 || ws.UserRating != 2 {
		t.Fatalf("watch clock blocked an unrated title: %+v %v", ws, err)
	}
	key = core.MovieKey(10)
	if err := s.Rate(ctx, 1, key, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRemote(ctx, 1, key, &watched, now.Add(-time.Hour), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	ws, err = s.Get(ctx, 1, key)
	if err != nil || ws.ViewCount != 1 || ws.UserRating != 10 {
		t.Fatalf("rating clock blocked an unwatched title: %+v %v", ws, err)
	}
}

func TestApplyRemoteNotifiesWithoutExport(t *testing.T) {
	ctx := context.Background()
	s := New(store.OpenTest(t), Options{})
	var got []Event
	s.Subscribe(func(ev Event) { got = append(got, ev) })
	key := core.MovieKey(4)
	watched := true
	if err := s.ApplyRemote(ctx, 6, key, &watched, time.Unix(10, 0), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != EventScrobble || !got[0].Imported || !got[0].Account {
		t.Fatalf("events %+v", got)
	}
	if _, ok, err := s.ClaimSync(ctx, "n", time.Now(), time.Minute); err != nil || ok {
		t.Fatalf("account import journaled an upload %v %v", ok, err)
	}
}

func TestStaleAttemptDoesNotReleaseReplacement(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db, Options{})
	key := core.MovieKey(3)
	if err := s.Scrobble(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	row, ok, err := s.ClaimSync(ctx, "node:a", t0, 2*time.Minute)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	locked, err := s.LockSync(ctx, 1, key, SyncWatch, "node:a", t0, 2*time.Minute)
	if err != nil || !locked {
		t.Fatal(err, locked)
	}
	t1 := t0.Add(3 * time.Minute)
	stolen, ok, err := s.ClaimSync(ctx, "node:b", t1, 2*time.Minute)
	if err != nil || !ok || stolen.Seq != row.Seq {
		t.Fatalf("replacement %+v %v %v", stolen, ok, err)
	}
	locked, err = s.LockSync(ctx, 1, key, SyncWatch, "node:b", t1, 2*time.Minute)
	if err != nil || !locked {
		t.Fatal(err, locked)
	}
	acked, err := s.AckSync(ctx, row.Seq, "node:a", t1)
	if err != nil || acked {
		t.Fatalf("stale ack %v %v", acked, err)
	}
	if err := s.UnlockSync(ctx, 1, key, SyncWatch, "node:a"); err != nil {
		t.Fatal(err)
	}
	var holder, claimed string
	if err := db.QueryRow(`SELECT holder FROM state_sync_lock WHERE user_id=1 AND rating_key=? AND aspect='watch'`, int64(key)).Scan(&holder); err != nil || holder != "node:b" {
		t.Fatalf("lock holder %q %v", holder, err)
	}
	if err := db.QueryRow(`SELECT claimed_by FROM state_sync_journal WHERE seq=?`, row.Seq).Scan(&claimed); err != nil || claimed != "node:b" {
		t.Fatalf("claim %q %v", claimed, err)
	}
	acked, err = s.AckSync(ctx, row.Seq, "node:b", t1)
	if err != nil || !acked {
		t.Fatalf("replacement ack %v %v", acked, err)
	}
}

func TestClaimSyncSkipsWriteTxWhenIdle(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db, Options{})
	statsCtx, qs := store.WithQueryStats(ctx)
	_, ok, err := s.ClaimSync(statsCtx, "node:a", time.Now(), 2*time.Minute)
	if err != nil || ok {
		t.Fatalf("ClaimSync = %v, %v", ok, err)
	}
	if qs.LockWait() > 0 || qs.Writes() > 0 {
		t.Fatalf("idle ClaimSync acquired write lock or wrote: lockWait=%s writes=%d", qs.LockWait(), qs.Writes())
	}
}
