package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

func pinTestApp(t *testing.T, accountID int64) *App {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(users.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/pins", "/api/v2/pins/1":
			fmt.Fprint(w, `{"id":1,"code":"fixture","authToken":"approved-token","expiresIn":600}`)
		case "/api/v2/user":
			fmt.Fprintf(w, `{"id":%d,"username":"approved"}`, accountID)
		case "/api/claim/token.json":
			fmt.Fprint(w, `{"token":"claim-42"}`)
		case "/api/claim/exchange":
			w.WriteHeader(http.StatusUnprocessableEntity)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	registrar, err := plextv.NewRegistrar(plextv.RegistrarOptions{Config: cfg, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return &App{cfg: cfg, users: users, registrar: registrar}
}

func TestWizardPINCannotReplaceExistingOwner(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		owner                  int64
		claimed, setup, reject bool
	}{
		{name: "first sign-in"},
		{name: "same claimed owner", owner: 42, claimed: true},
		{name: "reconnect after setup", owner: 42, claimed: true, setup: true},
		{name: "reconnect rejects different owner", owner: 99, claimed: true, setup: true, reject: true},
		{name: "different claimed owner", owner: 99, claimed: true, reject: true},
		{name: "authenticated transfer after unclaim", owner: 99, setup: true},
		{name: "correct initial account", owner: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := tc.owner
			a := pinTestApp(t, 42)
			if err := a.cfg.Update(func(c *config.Config) error {
				c.Plex.OwnerPlexID = owner
				if owner != 0 {
					c.Plex.Token = "existing-token"
					c.Plex.OwnerUsername = "existing"
					c.Plex.Claimed = tc.claimed
					c.SetupComplete = tc.setup
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			beforeConfig := a.cfg.Get()
			before := beforeConfig.Plex
			p := &plexAuth{app: a, pins: map[string]*plextv.PIN{"1": {ID: 1, Code: "fixture", ExpiresAt: time.Now().Add(time.Minute)}}}
			ok, err := p.PollPIN(context.Background(), "1")
			if tc.reject {
				if err == nil || ok {
					t.Fatalf("different account replaced owner: ok=%v err=%v", ok, err)
				}
				if p.pins["1"] != nil {
					t.Fatal("rejected owner PIN retained")
				}
				got := a.cfg.Get().Plex
				if got.OwnerPlexID != before.OwnerPlexID || got.Token != before.Token || got.OwnerUsername != before.OwnerUsername {
					t.Fatalf("owner config changed: %+v", got)
				}
			} else {
				if err != nil || !ok {
					t.Fatalf("owner approval failed: ok=%v err=%v", ok, err)
				}
				if a.cfg.Get().Plex.OwnerPlexID != 42 {
					t.Fatal("owner was not installed")
				}
			}
			// Reauthorizing rotates only account credentials, not the server
			// identity, setup state, source settings, or listening port.
			after := a.cfg.Get()
			after.Plex = beforeConfig.Plex
			if !reflect.DeepEqual(after, beforeConfig) {
				t.Fatal("PIN reauthorization changed non-Plex settings")
			}
		})
	}
}

func TestWizardClaimExchangeFailureStoresNoServerToken(t *testing.T) {
	ctx := context.Background()
	a := pinTestApp(t, 42)
	p := &plexAuth{app: a, pins: map[string]*plextv.PIN{"1": {ID: 1, Code: "fixture", ExpiresAt: time.Now().Add(time.Minute)}}}
	ok, err := p.PollPIN(ctx, "1")
	if err != nil || !ok {
		t.Fatalf("PollPIN: ok=%v err=%v", ok, err)
	}
	err = p.Claim(ctx)
	var exchangeErr *plextv.ClaimExchangeError
	if !errors.Is(err, plextv.ErrClaimExchangeFailed) || !errors.As(err, &exchangeErr) {
		t.Fatalf("Claim error = %v, want *ClaimExchangeError", err)
	}
	got := a.cfg.Get().Plex
	if got.Claimed || got.Token != "" {
		t.Fatalf("failed claim stored server token or claimed flag: %+v", got)
	}
	if _, err := a.users.Authenticate(ctx, "approved-token"); err == nil {
		t.Fatal("owner PIN token was stored as a server token")
	}
}

func TestWizardPrunesExpiredPINs(t *testing.T) {
	a := pinTestApp(t, 42)
	p := &plexAuth{app: a, pins: map[string]*plextv.PIN{"old": {ID: 9, Code: "expired", ExpiresAt: time.Now().Add(-time.Minute)}}}
	if _, _, _, err := p.StartPIN(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.pins) != 1 || p.pins["old"] != nil {
		t.Fatalf("expired requests retained: %d", len(p.pins))
	}
	p.pins["1"].ExpiresAt = time.Now().Add(-time.Minute)
	if ok, err := p.PollPIN(context.Background(), "1"); err == nil || ok {
		t.Fatal("expired PIN accepted")
	}
}

func TestPINRetentionIsBoundedWithoutUpstreamExpiry(t *testing.T) {
	pins := map[string]*plextv.PIN{}
	for n := 1; n <= 300; n++ {
		rememberPIN(pins, strconv.Itoa(n), &plextv.PIN{ID: int64(n), Code: "fixture"})
	}
	if len(pins) != 256 {
		t.Fatalf("retained %d PINs", len(pins))
	}
	for _, p := range pins {
		if p.ExpiresAt.IsZero() {
			t.Fatal("missing expiry")
		}
	}
	if pins["300"] == nil {
		t.Fatal("new sign-in was evicted")
	}
}
