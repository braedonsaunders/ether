package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func testViewerPushSubscription(t *testing.T, endpoint string) PushSubscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return PushSubscription{Endpoint: endpoint, Keys: PushKeys{Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16)), P256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())}}
}

func testPushGrants(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, id := range []int64{1, 2} {
		if _, err := db.Exec(`INSERT INTO auth_users(id,created_at,updated_at) VALUES(?,?,?)`, id, 0, 0); err != nil {
			t.Fatal(err)
		}
		_, err := db.Exec(`INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires,revoked) VALUES(?,?,?,'','','','',0,0,?,0)`, testGrant(id), id, "viewer-portal", int64(99999999999999))
		if err != nil {
			t.Fatal(err)
		}
	}
}
func testGrant(id int64) string { return strings.Repeat(string(rune('a'+id-1)), 64) }

func TestViewerPushIsolationClaimsAndRevocation(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	testPushGrants(t, db)
	now := time.Unix(1000, 0)
	active := map[int64]bool{1: true, 2: true}
	prefs := &ViewerAlerts{DB: db, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) {
		if !active[id] {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id}, nil
	}}
	var sent []string
	push := &ViewerPush{Notifications: New(db, Options{}), Preferences: prefs, Now: func() time.Time { return now }, Allowed: func(context.Context, *core.User, core.RatingKey) (bool, error) { return true, nil }, Send: func(_ context.Context, sub PushSubscription, _ Event) error {
		sent = append(sent, sub.Endpoint)
		return nil
	}}
	a, b := &core.User{ID: 1}, &core.User{ID: 2}
	if err := prefs.SetEnabled(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if err := prefs.SetEnabled(ctx, b, true); err != nil {
		t.Fatal(err)
	}
	one := testViewerPushSubscription(t, "https://push.test/one")
	two := testViewerPushSubscription(t, "https://push.test/two")
	if err := push.Register(ctx, a, one, testGrant(a.ID)); err != nil {
		t.Fatal(err)
	}
	if err := push.Register(ctx, b, two, testGrant(b.ID)); err != nil {
		t.Fatal(err)
	}
	if err := push.Register(ctx, b, one, testGrant(b.ID)); err == nil {
		t.Fatal("another viewer stole endpoint")
	}
	if ok, err := push.Registered(ctx, b, one.Endpoint); err != nil || ok {
		t.Fatal("cross-viewer endpoint visible", ok, err)
	}
	// An owner channel exists but must never receive viewer events.
	if _, err := db.ExecContext(ctx, `INSERT INTO notify_channels(id,config) VALUES('owner','{}')`); err != nil {
		t.Fatal(err)
	}
	key := core.EpisodeKey(3, 1, 2).String()
	if _, err := db.ExecContext(ctx, `INSERT INTO viewer_episode_alerts VALUES(?,?,?,?,?,?)`, 1, key, "Show", "Episode", 1001, time.Unix(1000, 0).UnixNano()); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(1002, 0)
	if err := push.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := push.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != one.Endpoint {
		t.Fatalf("viewer isolation or replay failed: %v", sent)
	}
	var ownerQueue int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_queue`).Scan(&ownerQueue); err != nil || ownerQueue != 0 {
		t.Fatalf("owner queue used: %d %v", ownerQueue, err)
	}
	active[1] = false
	if err := push.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	active[1] = true
	if ok, err := push.Registered(ctx, a, one.Endpoint); err != nil || ok {
		t.Fatal("inactive destination retained", ok, err)
	}
}

func TestViewerPushNoBacklogAndRechecksPolicy(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	testPushGrants(t, db)
	now := time.Unix(10000, 0)
	allowed := false
	sends := 0
	prefs := &ViewerAlerts{DB: db, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }}
	push := &ViewerPush{Notifications: New(db, Options{}), Preferences: prefs, Now: func() time.Time { return now }, Allowed: func(context.Context, *core.User, core.RatingKey) (bool, error) { return allowed, nil }, Send: func(context.Context, PushSubscription, Event) error { sends++; return nil }}
	u := &core.User{ID: 1}
	if err := prefs.SetEnabled(ctx, u, true); err != nil {
		t.Fatal(err)
	}
	sub := testViewerPushSubscription(t, "https://push.test/device")
	if err := push.Register(ctx, u, sub, testGrant(u.ID)); err != nil {
		t.Fatal(err)
	}
	for i, at := range []int64{9999, 10000, 10001} {
		if _, err := db.ExecContext(ctx, `INSERT INTO viewer_episode_alerts VALUES(?,?,?,?,?,?)`, 1, core.EpisodeKey(7, 1, i+1).String(), "Show", "Episode", at, time.Unix(10000, 0).UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	now = time.Unix(10002, 0)
	if err := push.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if sends != 0 {
		t.Fatal("policy ignored")
	}
	allowed = true
	if err := push.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("old alerts or policy failure: %d", sends)
	}
	now = now.Add(2 * time.Hour)
	if _, err := db.ExecContext(ctx, `INSERT INTO viewer_episode_alerts VALUES(?,?,?,?,?,?)`, 1, core.EpisodeKey(7, 1, 4).String(), "Show", "Episode", 10003, time.Unix(10000, 0).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := push.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("downtime backlog sent: %d", sends)
	}
}

func TestViewerPushClaimsAcrossWorkersAndPreferenceGeneration(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	testPushGrants(t, db)
	now := time.Unix(20000, 0)
	viewer := &core.User{ID: 1}
	prefs := &ViewerAlerts{DB: db, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }}
	var sends atomic.Int32
	makePush := func() *ViewerPush {
		return &ViewerPush{Notifications: New(db, Options{}), Preferences: prefs, Now: func() time.Time { return now }, Allowed: func(context.Context, *core.User, core.RatingKey) (bool, error) { return true, nil }, Send: func(context.Context, PushSubscription, Event) error { sends.Add(1); return nil }}
	}
	a, b := makePush(), makePush()
	if err := prefs.SetEnabled(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(ctx, viewer, testViewerPushSubscription(t, "https://push.test/atomic"), testGrant(viewer.ID)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := db.ExecContext(ctx, `INSERT INTO viewer_episode_alerts VALUES(?,?,?,?,?,?)`, viewer.ID, core.EpisodeKey(9, 1, 1).String(), "Show", "Episode", now.Unix(), time.Unix(20000, 0).UnixNano()); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() { errs <- a.Drain(ctx) }()
	go func() { errs <- b.Drain(ctx) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if sends.Load() != 1 {
		t.Fatalf("duplicate claims sent %d", sends.Load())
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO viewer_episode_alerts VALUES(?,?,?,?,?,?)`, viewer.ID, core.EpisodeKey(9, 1, 2).String(), "Show", "Episode", now.Unix(), time.Unix(20000, 0).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := prefs.SetEnabled(ctx, viewer, false); err != nil {
		t.Fatal(err)
	}
	if err := prefs.SetEnabled(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	if err := b.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 {
		t.Fatal("old opt-in generation delivered")
	}
}

func TestViewerPushRevocationAndDeactivationAreTransactional(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	testPushGrants(t, db)
	other := strings.Repeat("c", 64)
	if _, err := db.Exec(`INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires,revoked) VALUES(?,1,'viewer-portal','','','','',0,0,?,0)`, other, int64(99999999999999)); err != nil {
		t.Fatal(err)
	}
	prefs := &ViewerAlerts{DB: db, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }}
	push := &ViewerPush{Notifications: New(db, Options{}), Preferences: prefs}
	u := &core.User{ID: 1}
	first := testViewerPushSubscription(t, "https://push.test/a")
	second := testViewerPushSubscription(t, "https://push.test/b")
	if err := push.Register(ctx, u, first, testGrant(1)); err != nil {
		t.Fatal(err)
	}
	if err := push.Register(ctx, u, second, other); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE identity_grants SET revoked=1 WHERE hash=?`, testGrant(1)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM viewer_push_subscriptions WHERE viewer_id=1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("transactional revoke: %d %v", n, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM viewer_push_subscriptions WHERE viewer_id=1`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rollback: %d %v", n, err)
	}
	if _, err = db.Exec(`UPDATE identity_grants SET revoked=1 WHERE hash=?`, testGrant(1)); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM viewer_push_subscriptions WHERE viewer_id=1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("other device lost: %d %v", n, err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM viewer_push_subscriptions WHERE viewer_id=1`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deactivation: %d %v", n, err)
	}
}
