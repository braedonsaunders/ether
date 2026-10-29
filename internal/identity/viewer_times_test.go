package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestViewerTimesObserveActualEventsAndAuthorize(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	for _, row := range []struct{ id, owner int }{{1, 0}, {2, 0}, {3, 1}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,is_owner,created_at,updated_at) VALUES(?,?,?,?,?)`, row.id, row.id, row.owner, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(10000, 0).UTC()
	clock := func() time.Time { return now }
	times, err := identity.NewViewerTimes(db, clock)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := times.Get(ctx, 1, 1)
	if err != nil || !unknown.LastLogin.IsZero() || !unknown.LastActivity.IsZero() {
		t.Fatalf("unknown %+v %v", unknown, err)
	}
	if _, err = times.Get(ctx, 2, 1); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("cross viewer %v", err)
	}
	if _, err = times.Get(ctx, 3, 1); err != nil {
		t.Fatalf("owner %v", err)
	}
	if err = times.RecordOwnerLogin(ctx, 3); err != nil {
		t.Fatal(err)
	}
	ownerTimes, err := times.Get(ctx, 3, 3)
	if err != nil || !ownerTimes.LastLogin.Equal(now) {
		t.Fatalf("external owner login: %+v %v", ownerTimes, err)
	}
	if _, err = grants.Issue(ctx, 1, nil, "jellyfin", core.Device{}); err != nil {
		t.Fatal(err)
	}
	if _, err = grants.IssueOwnerLogin(ctx, 1, nil, "owner", core.Device{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("non-owner sign-in: %v", err)
	}
	if err = times.RecordOwnerLogin(ctx, 1); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("non-owner external sign-in: %v", err)
	}
	unknown, err = times.Get(ctx, 1, 1)
	if err != nil || !unknown.LastLogin.IsZero() || !unknown.LastActivity.IsZero() {
		t.Fatalf("mint invented event %+v %v", unknown, err)
	}
	if _, err = grants.IssueLogin(ctx, 1, nil, "jellyfin", core.Device{}); err != nil {
		t.Fatal(err)
	}
	got, err := times.Get(ctx, 1, 1)
	if err != nil || !got.LastLogin.Equal(now) || !got.LastActivity.Equal(now) {
		t.Fatalf("login %+v %v", got, err)
	}
	olderGrants, _ := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now.Add(-time.Hour) })
	if _, err = olderGrants.IssueLogin(ctx, 1, nil, "emby", core.Device{}); err != nil {
		t.Fatal(err)
	}
	if got, err = times.Get(ctx, 1, 1); err != nil || !got.LastLogin.Equal(now) {
		t.Fatalf("older login regressed: %+v %v", got, err)
	}
	now = now.Add(2 * time.Minute)
	if err = times.Observe(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = times.Get(ctx, 1, 1)
	if !got.LastActivity.Equal(now.Add(-2 * time.Minute)) {
		t.Fatal("activity wrote too frequently", got)
	}
	now = now.Add(4 * time.Minute)
	if err = times.Observe(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = times.Get(ctx, 1, 1)
	if !got.LastActivity.Equal(now) || !got.LastLogin.Equal(now.Add(-6*time.Minute)) {
		t.Fatal("activity or login changed incorrectly", got)
	}
	oldClock := func() time.Time { return now.Add(-time.Hour) }
	other, _ := identity.NewViewerTimes(db, oldClock)
	if err = other.Observe(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = times.Get(ctx, 1, 1)
	if !got.LastActivity.Equal(now) {
		t.Fatal("older node regressed activity", got)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET last_seen_at=? WHERE id=1`, now.Add(time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	got, _ = times.Get(ctx, 1, 1)
	if !got.LastActivity.Equal(now.Add(time.Minute)) {
		t.Fatal("Plex activity omitted", got)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	if _, err = times.Get(ctx, 3, 1); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive owner %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err = times.Get(ctx, 1, 1); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive viewer %v", err)
	}
}

func TestQuickConnectRecordsOnlySuccessfulRedemption(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0).UTC()
	clock := func() time.Time { return now }
	q, _ := identity.NewQuickConnect(db, time.Minute, time.Hour, clock)
	times, _ := identity.NewViewerTimes(db, clock)
	code, secret, err := q.Initiate(ctx, "jellyfin", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.Poll(ctx, secret); !errors.Is(err, identity.ErrQuickConnectPending) {
		t.Fatal(err)
	}
	if err = q.Approve(ctx, code, 1); err != nil {
		t.Fatal(err)
	}
	before, err := times.Get(ctx, 1, 1)
	if err != nil || !before.LastLogin.IsZero() {
		t.Fatalf("approval counted login: %+v %v", before, err)
	}
	now = now.Add(time.Second)
	if _, err = q.Poll(ctx, secret); err != nil {
		t.Fatal(err)
	}
	after, err := times.Get(ctx, 1, 1)
	if err != nil || !after.LastLogin.Equal(now) || !after.LastActivity.Equal(now) {
		t.Fatalf("redemption %+v %v", after, err)
	}
}

func TestRefreshDoesNotChangeLastLogin(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0).UTC()
	clock := func() time.Time { return now }
	grants, _ := identity.NewGrants(db, members{db}, time.Hour, clock)
	times, _ := identity.NewViewerTimes(db, clock)
	device := core.Device{ID: "tv"}
	access, err := grants.IssueLogin(ctx, 1, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := grants.IssueRefresh(ctx, "silo", access, device)
	if err != nil {
		t.Fatal(err)
	}
	login := now
	now = now.Add(time.Minute)
	if _, _, err = grants.Refresh(ctx, "silo", refresh, device); err != nil {
		t.Fatal(err)
	}
	result, err := times.Get(ctx, 1, 1)
	if err != nil || !result.LastLogin.Equal(login) {
		t.Fatalf("refresh counted as login: %+v %v", result, err)
	}
}

func TestViewerTimesConcurrentNodesNeverRegress(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	older := time.Unix(10000, 0).UTC()
	newer := older.Add(10 * time.Minute)
	a, _ := identity.NewViewerTimes(db, func() time.Time { return older })
	b, _ := identity.NewViewerTimes(db, func() time.Time { return newer })
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, svc := range []*identity.ViewerTimes{a, b} {
		wg.Add(1)
		go func(svc *identity.ViewerTimes) { defer wg.Done(); errs <- svc.Observe(ctx, 1) }(svc)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := a.Get(ctx, 1, 1)
	if err != nil || !result.LastActivity.Equal(newer) {
		t.Fatalf("activity=%+v err=%v", result, err)
	}
}

func TestLoginRecordingFailureRollsBackCredential(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, members{db}, time.Hour, nil)
	q, _ := identity.NewQuickConnect(db, time.Minute, time.Hour, nil)
	code, secret, err := q.Initiate(ctx, "jellyfin", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Approve(ctx, code, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `DROP TABLE identity_viewer_times`); err != nil {
		t.Fatal(err)
	}
	if _, err = grants.IssueLogin(ctx, 1, nil, "jellyfin", core.Device{}); err == nil {
		t.Fatal("issued login without its durable observation")
	}
	if _, err = q.Poll(ctx, secret); err == nil {
		t.Fatal("redeemed quick connect without its durable observation")
	}
	var count, consumed int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_grants`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("grants=%d err=%v", count, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT consumed FROM identity_quickconnect`).Scan(&consumed); err != nil || consumed != 0 {
		t.Fatalf("consumed=%d err=%v", consumed, err)
	}
}
