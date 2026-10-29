package identity

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestLoginLimiterBuckets(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(1000, 0)
	l, err := NewLoginLimiter(db, time.Minute, 3, 2, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ip := netip.MustParseAddr("192.0.2.1")
	check := func(id string, want bool) {
		t.Helper()
		got, err := l.Allow(ctx, id, ip)
		if err != nil || got != want {
			t.Fatalf("%s: got %v, %v; want %v", id, got, err, want)
		}
	}
	check("alice", true)
	check("alice", true)
	check("alice", false)
	// Even if either prior login succeeded, there is no success reset; attempts
	// for another username still encounter the exhausted IP bucket.
	check("bob", false)
	now = now.Add(time.Minute)
	check("bob", true)
	if ok, err := l.Allow(ctx, "alice", netip.MustParseAddr("192.0.2.2")); err != nil || !ok {
		t.Fatalf("other IP: %v %v", ok, err)
	}
}

func TestLoginLimiterCleanupIsBounded(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(1000, 0)
	l, err := NewLoginLimiter(db, time.Minute, 100, 100, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"a", "b", "c"} {
		if _, err = db.ExecContext(ctx, `INSERT INTO identity_rate_buckets(bucket_key,window_start,attempts) VALUES(?,?,?)`, key, now.Add(-time.Hour).UnixMilli(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO identity_rate_buckets(bucket_key,window_start,attempts) VALUES(?,?,?)`, "active", now.UnixMilli(), 1); err != nil {
		t.Fatal(err)
	}
	n, err := l.Cleanup(ctx, 2)
	if err != nil || n != 2 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM identity_rate_buckets`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("remaining: %d %v", count, err)
	}
}
