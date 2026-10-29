package simkl

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestLeaseExclusionAndFencedRelease(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := TokenStore{DB: db, Sealer: testSealer{}}
	token := Token{ClientID: "id", AccessToken: "a", RefreshToken: "r", ExpiresIn: 300, Scope: "media:read media:write"}
	now := time.Now()
	if err := s.Save(ctx, 1, token, now); err != nil {
		t.Fatal(err)
	}
	first, err := s.AcquireLease(ctx, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(ctx, 1, now); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent claim: %v", err)
	}
	second, err := s.AcquireLease(ctx, 1, now.Add(91*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseLease(ctx, 1, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLease(ctx, 1, now.Add(92*time.Second)); !errors.Is(err, ErrBusy) {
		t.Fatalf("old owner released new lease: %v", err)
	}
	if err := s.ReleaseLease(ctx, 1, second); err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectDeleteRechecksLease(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := TokenStore{DB: db, Sealer: testSealer{}}
	now := time.Now()
	token := Token{ClientID: "id", AccessToken: "a", RefreshToken: "r", ExpiresIn: 300, Scope: "media:read media:write"}
	if err := s.Save(ctx, 1, token, now); err != nil {
		t.Fatal(err)
	}
	var observed int64
	var access, refresh string
	if err := db.QueryRowContext(ctx, `SELECT lease_until,access_token,refresh_token FROM simkl_tokens WHERE user_id=?`, 1).Scan(&observed, &access, &refresh); err != nil || observed >= now.UnixMilli() {
		t.Fatalf("initial lease: %d %v", observed, err)
	}
	if _, err := s.AcquireLease(ctx, 1, now); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := deleteUnleasedGrant(ctx, tx, 1, "id", access, refresh, now); !errors.Is(err, ErrBusy) {
		t.Fatalf("stale lease observation allowed delete: %v", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM simkl_tokens WHERE user_id=?`, 1).Scan(&count); err != nil || count != 1 {
		t.Fatalf("grant removed: %d %v", count, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE simkl_tokens SET lease_until=0,refresh_token=? WHERE user_id=?`, "rotated", 1); err != nil {
		t.Fatal(err)
	}
	if err := deleteUnleasedGrant(ctx, tx, 1, "id", access, refresh, now); !errors.Is(err, ErrBusy) {
		t.Fatalf("stale credentials allowed delete: %v", err)
	}
}

func TestRefreshRequiresLeaseAndKeepsAccount(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := TokenStore{DB: db, Sealer: testSealer{}}
	tok := Token{ClientID: "id", AccessToken: "a", RefreshToken: "r", ExpiresIn: 300, Scope: "media:read media:write"}
	if err := s.Save(ctx, 1, tok, time.Now()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"access_token":"new","refresh_token":"r","expires_in":300,"scope":"media:read media:write"}`))
	}))
	defer srv.Close()
	auth := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	if _, err := s.Refresh(ctx, 1, "wrong", auth); !errors.Is(err, ErrBusy) || calls != 0 {
		t.Fatalf("unleased refresh: %v", err)
	}
	id, err := s.AcquireLease(ctx, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.Refresh(ctx, 1, id, auth)
	if err != nil || grant.Access != "new" {
		t.Fatalf("refresh: %+v %v", grant, err)
	}
	loaded, err := s.Load(ctx, 1)
	if err != nil || loaded.Access != "new" {
		t.Fatalf("stored: %+v %v", loaded, err)
	}
	if err := s.Delete(ctx, 1); !errors.Is(err, ErrBusy) {
		t.Fatalf("disconnect bypassed lease: %v", err)
	}
}
