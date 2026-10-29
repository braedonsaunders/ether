package app

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
)

func TestDLNARequiresSelectedActiveViewerAndVerifiedLAN(t *testing.T) {
	a := setupTestApp(t, true)
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(context.Background(), "viewer", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(context.Background()); err != nil {
		t.Fatal(err)
	}
	settings := frontend.Settings{"viewerId": float64(id)}
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "192.168.1.20:1234"
	if viewer, err := a.dlnaViewer(context.Background(), request, settings); err != nil || viewer.ID != id {
		t.Fatal(viewer, err)
	}
	if _, err := a.dlnaViewer(context.Background(), request, frontend.Settings{}); err == nil {
		t.Fatal("no selection accepted")
	}
	request.RemoteAddr = "203.0.113.10:1234"
	request.Header.Set("X-Forwarded-For", "192.168.1.20")
	if _, err := a.dlnaViewer(context.Background(), request, settings); err == nil {
		t.Fatal("untrusted proxy spoofed LAN")
	}
	if err := a.cfg.Update(func(c *config.Config) error { c.Server.TrustedProxyCIDRs = []string{"192.168.1.0/24"}; return nil }); err != nil {
		t.Fatal(err)
	}
	request.RemoteAddr = "192.168.1.2:1234"
	request.Header.Set("X-Forwarded-For", "8.8.8.8")
	if _, err := a.dlnaViewer(context.Background(), request, settings); err == nil {
		t.Fatal("remote viewer behind trusted proxy accepted")
	}
}
