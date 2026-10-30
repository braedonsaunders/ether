package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestOwnerSessionIsHashedScopedAndRevocable(t *testing.T) {
	s, v, _ := setup(t)
	ctx := context.Background()
	token, expires, err := s.IssueOwnerSession(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	if !IsOwnerSession(token) || strings.Contains(token, "server-token") {
		t.Fatalf("session %q", token)
	}
	if got := expires.Sub(s.o.Now()); got != OwnerSessionTTL {
		t.Fatalf("expiry %v", got)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM auth_sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != HashToken(token) || strings.Contains(stored, token) {
		t.Fatal("session stored in the clear")
	}
	u, err := s.OwnerSession(ctx, token)
	if err != nil || !u.IsOwner || u.Username != "owner" {
		t.Fatalf("owner session: %+v %v", u, err)
	}
	calls := v.calls.Load()
	// Never a Plex or playback credential, and never sent to plex.tv.
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("session accepted as a Plex token: %v", err)
	}
	if v.calls.Load() != calls {
		t.Fatal("session forwarded to plex.tv")
	}
	if err := s.RevokeOwnerSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OwnerSession(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked session accepted: %v", err)
	}
	if err := s.RevokeOwnerSession(ctx, token); err != nil {
		t.Fatalf("second sign-out: %v", err)
	}
}

func TestOwnerSessionRequiresOwnerCredential(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	for _, cred := range []string{"", "alice-access", "nope"} {
		if _, _, err := s.IssueOwnerSession(ctx, cred); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("%q: %v", cred, err)
		}
	}
	session, _, err := s.IssueOwnerSession(ctx, "owner-access")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssueOwnerSession(ctx, session); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("session extended itself: %v", err)
	}
	owner, err := s.Authenticate(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	delegated, err := s.IssueDelegation(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssueOwnerSession(ctx, delegated); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("delegation exchanged: %v", err)
	}
	if _, err := s.OwnerSession(ctx, "server-token"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("plex token accepted as a session: %v", err)
	}
	if _, err := s.OwnerSession(ctx, ownerSessionPrefix+"forged"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("forged session: %v", err)
	}
}

func TestOwnerSessionExpiryRenewalAndMaxAge(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	s.o.Now = func() time.Time { return now }
	token, _, err := s.IssueOwnerSession(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	// Daily use keeps the session alive...
	for day := 0; day < 80; day++ {
		now = now.Add(24 * time.Hour)
		if _, err := s.OwnerSession(ctx, token); err != nil {
			t.Fatalf("day %d: %v", day, err)
		}
	}
	// ...but never past its absolute lifetime.
	now = now.Add(11 * 24 * time.Hour)
	if _, err := s.OwnerSession(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("session outlived max age: %v", err)
	}

	idle, _, err := s.IssueOwnerSession(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(OwnerSessionTTL)
	if _, err := s.OwnerSession(ctx, idle); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("idle session survived: %v", err)
	}
}

func TestOwnerSessionsAreBounded(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	s.o.Now = func() time.Time { return now }
	first, _, err := s.IssueOwnerSession(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxOwnerSessions+5; i++ {
		now = now.Add(time.Second)
		if _, _, err := s.IssueOwnerSession(ctx, "server-token"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil || n != maxOwnerSessions {
		t.Fatalf("sessions=%d err=%v", n, err)
	}
	if _, err := s.OwnerSession(ctx, first); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("oldest session kept: %v", err)
	}
}

func TestOwnerSessionEndsWhenOwnershipChanges(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	token, _, err := s.IssueOwnerSession(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	// Reconnecting the same owner keeps the session.
	if err := s.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "rotated-server-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OwnerSession(ctx, token); err != nil {
		t.Fatalf("same-owner reconnect: %v", err)
	}
	if err := s.SetOwner(ctx, plextv.Account{ID: 77, Username: "new-owner"}, "new-server-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OwnerSession(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("former owner's session accepted: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("former owner's sessions kept: %d %v", n, err)
	}
}

func TestOwnerSessionAcrossClusterNodes(t *testing.T) {
	a, b := twoNodes(t, nil)
	ctx := context.Background()
	if err := a.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "server-token"); err != nil {
		t.Fatal(err)
	}
	if err := b.reload(ctx); err != nil {
		t.Fatal(err)
	}
	token, _, err := a.IssueOwnerSession(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	if u, err := b.OwnerSession(ctx, token); err != nil || !u.IsOwner {
		t.Fatalf("other node: %+v %v", u, err)
	}
	if err := b.RevokeOwnerSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := a.OwnerSession(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revocation on another node ignored: %v", err)
	}
}

// Concurrent sign-ins at the cap never leave more than maxOwnerSessions.
// With ETHER_TEST_POSTGRES the goroutines use separate pooled connections,
// which is where the owner-row lock matters.
func TestOwnerSessionCapHoldsUnderConcurrentIssuance(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	for i := 0; i < maxOwnerSessions-1; i++ {
		if _, _, err := s.IssueOwnerSession(ctx, "server-token"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.IssueOwnerSession(ctx, "server-token")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil || n != maxOwnerSessions {
		t.Fatalf("sessions=%d err=%v", n, err)
	}
}
