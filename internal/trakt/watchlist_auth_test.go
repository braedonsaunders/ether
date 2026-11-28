package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestWatchlistNotConnectedReturnsSentinel(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	c := New(Options{
		Config: config.Trakt{Enabled: true, ClientID: "cid"},
		DB:     db,
	})
	if _, err := c.Watchlist(context.Background(), 42, 100); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("unlinked viewer: err = %v, want ErrNotConnected", err)
	}

	cDisabled := New(Options{
		Config: config.Trakt{Enabled: false, ClientID: "cid"},
		DB:     db,
	})
	if _, err := cDisabled.Watchlist(context.Background(), 42, 100); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("disabled client: err = %v, want ErrNotConnected", err)
	}
}

func TestRefreshNonInvalidGrantDoesNotRevokeTokens(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, tc := range []struct {
		name         string
		oauthErr     string
		wantRevoked  bool
		wantKeptInDB bool
	}{
		{"invalid_client keeps tokens", "invalid_client", false, true},
		{"invalid_grant revokes tokens", "invalid_grant", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.oauthErr})
			}))
			defer srv.Close()

			now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
			c := New(Options{
				Config:          config.Trakt{Enabled: true, ClientID: "cid", ClientSecret: "sec"},
				DB:              db,
				BaseURL:         srv.URL,
				HTTPClient:      srv.Client(),
				Now:             func() time.Time { return now },
				MinPostInterval: -1,
			})
			tok := Tokens{
				UserID:       7,
				AccessToken:  "expired-access",
				RefreshToken: "keep-refresh",
				ExpiresAt:    now.Add(-time.Hour),
				Username:     "alice",
			}
			if err := c.saveTokens(context.Background(), tok); err != nil {
				t.Fatal(err)
			}
			loaded, ok, err := c.Tokens(context.Background(), 7)
			if err != nil || !ok {
				t.Fatalf("load tokens: ok=%v err=%v", ok, err)
			}
			_, err = c.refreshFrom(context.Background(), loaded)
			if errors.Is(err, ErrRevoked) != tc.wantRevoked {
				t.Fatalf("refreshFrom err = %v, wantRevoked=%v", err, tc.wantRevoked)
			}
			_, stillThere, err := c.Tokens(context.Background(), 7)
			if err != nil {
				t.Fatal(err)
			}
			if stillThere != tc.wantKeptInDB {
				t.Fatalf("tokens kept in DB = %v, want %v", stillThere, tc.wantKeptInDB)
			}
		})
	}
}
