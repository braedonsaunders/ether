package simkl

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncPersistsRateLimit(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tokens := TokenStore{DB: db, Sealer: testSealer{}}
	token := Token{ClientID: "id", AccessToken: "a", RefreshToken: "r", ExpiresIn: 7 * 24 * 3600, Scope: "media:read media:write"}
	if err := tokens.Save(ctx, 1, token, time.Now()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	s := Service{Auth: AuthClient{BaseURL: srv.URL, ClientID: "id"}, Tokens: tokens, State: state.New(db, state.Options{})}
	if _, err := s.Sync(ctx, 1); err == nil {
		t.Fatal("rate limit ignored")
	}
	if _, err := s.Sync(ctx, 1); !errors.Is(err, ErrBusy) || calls != 1 {
		t.Fatalf("backoff failed: %v calls=%d", err, calls)
	}
}

func TestRevokedRefreshRequiresReconnect(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tokens := TokenStore{DB: db, Sealer: testSealer{}}
	tok := Token{ClientID: "id", AccessToken: "a", RefreshToken: "r", ExpiresIn: 1, Scope: "media:read media:write"}
	if err := tokens.Save(ctx, 1, tok, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	svc := Service{Auth: AuthClient{BaseURL: srv.URL, ClientID: "id"}, Tokens: tokens, State: state.New(db, state.Options{})}
	if _, err := svc.Sync(ctx, 1); err == nil {
		t.Fatal("revocation ignored")
	}
	var warning string
	if err := db.QueryRow(`SELECT last_warning FROM simkl_tokens WHERE user_id=1`).Scan(&warning); err != nil || warning != "auth_revoked" {
		t.Fatalf("warning %q: %v", warning, err)
	}
}
