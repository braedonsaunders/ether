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
	poll, _ := identity.NewPollLimiter(a.db, time.Minute, 100, 20, nil)
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
	code, secret, err := jf.QuickConnectInitiate(ctx, core.Device{ID: "tablet", DeviceName: "Family tablet", Client: "Jellyfin", Product: "Jellyfin TV", Version: "1.2"})
	if err != nil {
		t.Fatal(err)
	}
	details, err := jf.QuickConnectDetails(ctx, secret)
	if err != nil || details.Approved || details.Code != code || details.Secret != secret || details.Device.DeviceName != "Family tablet" || details.Device.Client != "Jellyfin" || details.CreatedAt.IsZero() {
		t.Fatal(details, err)
	}
	if _, err := emby.QuickConnectDetails(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-surface details", err)
	}
	if approved, err := jf.QuickConnectStatus(ctx, secret); err != nil || approved {
		t.Fatal("pending status", approved, err)
	}
	if _, err := emby.QuickConnectStatus(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-surface status", err)
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
	if details, err := jf.QuickConnectDetails(ctx, secret); err != nil || !details.Approved || details.Code != code {
		t.Fatal("approved details", details, err)
	}
	for i := 0; i < 2; i++ {
		if approved, err := jf.QuickConnectStatus(ctx, secret); err != nil || !approved {
			t.Fatal("approved status", approved, err)
		}
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
	if _, err := jf.QuickConnectStatus(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("consumed status", err)
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

func TestPublicViewerPickerRequiresOptInAndOnlyActiveLocalCredentials(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if _, err := a.db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at,active) VALUES(11,111,0,0,1),(12,112,0,0,0),(13,113,0,0,1),(14,114,0,0,1)`); err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(a.db)
	for _, item := range []struct {
		id   int64
		name string
	}{{11, "plex-linked"}, {12, "inactive"}, {14, "local-viewer"}} {
		if err := local.Create(ctx, item.id, item.name, "password"); err != nil {
			t.Fatal(err)
		}
	}
	limiter, _ := identity.NewLoginLimiter(a.db, time.Minute, 30, 8, nil)
	enabled := false
	s := surfaceIdentity{surface: "jellyfin", local: local, login: limiter, settings: func() frontend.Settings { return frontend.Settings{"publicViewerPicker": enabled} }}
	if list, err := s.PublicViewers(ctx); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	enabled = true
	if _, err := s.PublicViewers(ctx); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("missing IP", err)
	}
	ctx = frontend.WithClientIP(ctx, netip.MustParseAddr("192.0.2.42"))
	viewers, err := s.PublicViewers(ctx)
	if err != nil || len(viewers) != 2 {
		t.Fatal(viewers, err)
	}
	if viewers[0].Name != "local-viewer" || viewers[1].Name != "plex-linked" {
		t.Fatal(viewers)
	}
}

func TestViewerTimesRequiresSurfaceCredentialAndActiveOwner(t *testing.T) {
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
	viewerID, err := local.CreateViewer(ctx, "member-times", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := identity.NewResolver(a.users, grants)
	if err != nil {
		t.Fatal(err)
	}
	times, err := identity.NewViewerTimes(a.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := surfaceIdentity{surface: "jellyfin", resolver: resolver, grants: grants, times: times}
	ownerGrant, err := grants.Issue(ctx, owner.ID, nil, "jellyfin", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	memberGrant, err := grants.Issue(ctx, viewerID, nil, "jellyfin", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ViewerTimes(ctx, memberGrant, owner.ID); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("member read owner: %v", err)
	}
	result, err := s.ViewerTimes(ctx, memberGrant, viewerID)
	if err != nil || !result.LastLogin.IsZero() || result.LastActivity.IsZero() {
		t.Fatalf("self: %+v %v", result, err)
	}
	if _, err = s.ViewerTimes(ctx, ownerGrant, viewerID); err != nil {
		t.Fatalf("owner: %v", err)
	}
	wrong := s
	wrong.surface = "emby"
	if _, err = wrong.ViewerTimes(ctx, ownerGrant, viewerID); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("cross surface: %v", err)
	}
	if _, err = a.db.ExecContext(ctx, `UPDATE auth_users SET is_owner=0 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ViewerTimes(ctx, ownerGrant, viewerID); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("stale owner: %v", err)
	}
	if err = s.Revoke(ctx, memberGrant); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ViewerTimes(ctx, memberGrant, viewerID); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked credential: %v", err)
	}
}

func TestSurfaceRefreshObservesActivityWithoutNewLogin(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	local, _ := identity.NewLocal(a.db)
	viewerID, err := local.CreateViewer(ctx, "refresh-times", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	clock := func() time.Time { return now }
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := identity.NewResolver(a.users, grants)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := identity.NewPollLimiter(a.db, time.Minute, 100, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := surfaceIdentity{surface: "silo", resolver: resolver, grants: grants, times: grants.ViewerTimes(), poll: limiter}
	device := core.Device{ID: "tv"}
	access, err := grants.IssueLogin(ctx, viewerID, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := s.IssueRefresh(ctx, access, device)
	if err != nil {
		t.Fatal(err)
	}
	login := now
	now = now.Add(6 * time.Minute)
	ctx = frontend.WithClientIP(ctx, netip.MustParseAddr("192.0.2.3"))
	access, _, err = s.Refresh(ctx, refresh, device)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ViewerTimes(ctx, access, viewerID)
	if err != nil || !got.LastLogin.Equal(login) || !got.LastActivity.Equal(now) {
		t.Fatalf("timestamps=%+v err=%v", got, err)
	}
}

func TestDeviceIdentityBindsSurfaceAndRejectsPlex(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := local.CreateViewer(ctx, "device-check", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "apple-tv"}
	token, err := grants.Issue(ctx, viewerID, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	surface := surfaceIdentity{surface: "silo", grants: grants}
	viewer, expires, err := surface.ResolveDevice(ctx, token, device)
	if err != nil || viewer == nil || viewer.ID != viewerID || !expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("viewer=%+v expires=%s err=%v", viewer, expires, err)
	}
	surface.surface = "emby"
	if _, _, err = surface.ResolveDevice(ctx, token, device); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("cross surface: %v", err)
	}
	surface.surface = "plex"
	if _, _, err = surface.ResolveDevice(ctx, token, device); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("Plex grant: %v", err)
	}
}
