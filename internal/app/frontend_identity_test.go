package app

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestFrontendClientIPAcceptsPublicPeersAndRejectsSpoofedHeaders(t *testing.T) {
	a := setupTestApp(t, true)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.4:1234"
	req.Header.Set("X-Forwarded-For", "192.168.1.5")
	ip, ok := a.frontendClientIP(req)
	if !ok || ip.String() != "198.51.100.4" {
		t.Fatal(ip, ok)
	}
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	ip, ok = a.frontendClientIP(req)
	if !ok || ip.String() != "198.51.100.9" {
		t.Fatal(ip, ok)
	}
	req.Header.Set("X-Forwarded-For", "invalid")
	if ip, ok = a.frontendClientIP(req); ok || ip.IsValid() {
		t.Fatal(ip, ok)
	}
}

func TestSurfaceIdentityLoginQuickConnectAndRevoke(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Create(ctx, owner.ID, "local-owner", "correct-password"); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(a.db, a.users, time.Hour, nil)
	resolver, _ := identity.NewResolver(a.users, grants)
	login, _ := identity.NewLoginLimiter(a.db, time.Minute, 100, 10, nil)
	poll, _ := identity.NewPollLimiter(a.db, time.Minute, 100, 10, nil)
	quick, _ := identity.NewQuickConnect(a.db, time.Minute, time.Hour, nil)
	jf := surfaceIdentity{surface: "jellyfin", resolver: resolver, local: local, grants: grants, quick: quick, login: login, poll: poll}
	emby := jf
	emby.surface = "emby"
	ctx = frontend.WithClientIP(ctx, netip.MustParseAddr("192.0.2.8"))
	if !jf.SetupComplete() {
		t.Fatal("protocol setup incomplete")
	}
	if _, err := jf.SignIn(context.Background(), "local-owner", "correct-password", core.Device{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("missing peer: %v", err)
	}
	if _, err := jf.SignIn(ctx, "local-owner", "wrong", core.Device{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("bad password: %v", err)
	}
	token, err := jf.SignIn(ctx, "local-owner", "correct-password", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := jf.Resolve(ctx, token)
	if err != nil || viewer.ID != owner.ID {
		t.Fatal(viewer, err)
	}
	if _, err := emby.Resolve(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-surface grant", err)
	}
	if err := emby.Revoke(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-surface revoke", err)
	}
	code, secret, err := jf.QuickConnectInitiate(ctx, core.Device{ID: "tablet"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jf.QuickConnectPoll(ctx, secret); !errors.Is(err, frontend.ErrQuickConnectPending) {
		t.Fatal(err)
	}
	if err := emby.QuickConnectApprove(ctx, viewer, code); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-surface approval", err)
	}
	if err := jf.QuickConnectApprove(ctx, viewer, code); err != nil {
		t.Fatal(err)
	}
	if _, err := emby.QuickConnectPoll(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-surface poll", err)
	}
	quickGrant, err := jf.QuickConnectPoll(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jf.Resolve(ctx, quickGrant); err != nil {
		t.Fatal(err)
	}
	if _, err := jf.QuickConnectPoll(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("reused secret", err)
	}
	if err := jf.Revoke(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := jf.Resolve(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("revoked grant resolved", err)
	}
	plex := jf
	plex.surface = "plex"
	if _, err := plex.SignIn(ctx, "local-owner", "correct-password", core.Device{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("Plex accepted local grant", err)
	}
}

func TestSurfaceIdentityRateLimits(t *testing.T) {
	a := setupTestApp(t, true)
	local, _ := identity.NewLocal(a.db)
	login, _ := identity.NewLoginLimiter(a.db, time.Minute, 20, 1, nil)
	poll, _ := identity.NewPollLimiter(a.db, time.Minute, 20, 1, nil)
	quick, _ := identity.NewQuickConnect(a.db, time.Minute, time.Hour, nil)
	s := surfaceIdentity{surface: "jellyfin", local: local, login: login, poll: poll, quick: quick}
	ctx := frontend.WithClientIP(context.Background(), netip.MustParseAddr("192.0.2.11"))
	if _, err := s.SignIn(ctx, "absent", "bad", core.Device{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.SignIn(ctx, "absent", "bad", core.Device{}); !errors.Is(err, frontend.ErrRateLimited) {
		t.Fatal(err)
	}
	_, secret, err := s.QuickConnectInitiate(ctx, core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.QuickConnectPoll(ctx, secret); !errors.Is(err, frontend.ErrQuickConnectPending) {
		t.Fatal(err)
	}
	if _, err := s.QuickConnectPoll(ctx, secret); !errors.Is(err, frontend.ErrRateLimited) {
		t.Fatal(err)
	}
}
