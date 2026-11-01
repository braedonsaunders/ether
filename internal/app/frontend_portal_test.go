package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/remote"
)

func TestPortalAddresses(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.1.0/24")
	lan := []remote.InterfaceAddr{{IP: net.ParseIP("192.168.1.5"), Net: network}}
	got, err := portalLANAddress(netip.MustParseAddr("192.168.1.9"), "0.0.0.0", "32400", lan)
	if err != nil || got != "http://192.168.1.5:32400/ether/me" {
		t.Fatalf("LAN %q %v", got, err)
	}
	if _, err = portalLANAddress(netip.MustParseAddr("192.168.2.9"), "0.0.0.0", "32400", lan); err == nil {
		t.Fatal("unreachable subnet selected")
	}
	if _, err = portalLANAddress(netip.MustParseAddr("192.168.1.9"), "192.168.1.6", "32400", lan); err == nil {
		t.Fatal("unbound interface selected")
	}
	candidates := []remote.Connection{{URI: "http://192.168.1.5:32400", Local: true}, {URI: "http://172.17.0.1:32400"}, {URI: "https://8.8.8.8:443"}}
	got, err = portalPublicAddress(context.Background(), candidates)
	if err != nil || got != "https://8.8.8.8:443/ether/me" {
		t.Fatalf("public %q %v", got, err)
	}
}

func TestPortalAndSurfaceBridgeModeUsesRequestHostAndAppPort(t *testing.T) {
	_, bridgeNet, _ := net.ParseCIDR("172.20.0.0/16")
	containerInterfaces := []remote.InterfaceAddr{{IP: net.ParseIP("172.20.0.2"), Net: bridgeNet}}
	peer := netip.MustParseAddr("192.168.68.10")

	req := httptest.NewRequest(http.MethodPost, "http://192.168.68.66:33200/ether/api/me/apps/jellyfin", nil)
	req.RemoteAddr = "172.20.0.1:54321"
	got, err := portalAddress(context.Background(), req, peer, "0.0.0.0", "33200", config.Config{}, nil, containerInterfaces, nil)
	if err != nil || got != "http://192.168.68.66:33200/ether/me" {
		t.Fatalf("bridge portal: got %q err %v", got, err)
	}

	local := &net.TCPAddr{IP: net.ParseIP("172.20.0.2"), Port: 8096}
	gotSurface, ok := surfaceAddress(req, peer, local, []string{"0.0.0.0:8096"}, []string{""}, nil, containerInterfaces)
	if !ok || gotSurface != "http://192.168.68.66:8096" {
		t.Fatalf("bridge jellyfin listener: got %q ok %v", gotSurface, ok)
	}
}

func TestPortalAndSurfaceReverseProxyHonoursTrustedHeaders(t *testing.T) {
	_, bridgeNet, _ := net.ParseCIDR("172.20.0.0/16")
	containerInterfaces := []remote.InterfaceAddr{{IP: net.ParseIP("172.20.0.2"), Net: bridgeNet}}
	trusted := []netip.Prefix{netip.MustParsePrefix("172.20.0.0/16")}
	peer := netip.MustParseAddr("198.51.100.25")

	req := httptest.NewRequest(http.MethodPost, "http://172.20.0.2:32400/ether/api/me/apps/jellyfin", nil)
	req.RemoteAddr = "172.20.0.5:41000"
	req.Header.Set("X-Forwarded-Host", "ether.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")

	got, err := portalAddress(context.Background(), req, peer, "0.0.0.0", "32400", config.Config{}, trusted, containerInterfaces, nil)
	if err != nil || got != "https://ether.example.com/ether/me" {
		t.Fatalf("trusted proxy portal: got %q err %v", got, err)
	}

	local := &net.TCPAddr{IP: net.ParseIP("172.20.0.2"), Port: 8096}
	gotSurface, ok := surfaceAddress(req, peer, local, []string{"0.0.0.0:8096"}, []string{""}, trusted, containerInterfaces)
	if !ok || gotSurface != "https://ether.example.com" {
		t.Fatalf("trusted proxy surface: got %q ok %v", gotSurface, ok)
	}

	// Untrusted proxy headers must be ignored.
	untrustedReq := httptest.NewRequest(http.MethodPost, "http://192.168.68.66:32400/ether/api/me/apps/jellyfin", nil)
	untrustedReq.RemoteAddr = "192.168.68.10:41000"
	untrustedReq.Header.Set("X-Forwarded-Host", "evil.example.com")
	untrustedReq.Header.Set("X-Forwarded-Proto", "https")

	got, err = portalAddress(context.Background(), untrustedReq, netip.MustParseAddr("192.168.68.10"), "0.0.0.0", "32400", config.Config{}, trusted, containerInterfaces, nil)
	if err != nil || got != "http://192.168.68.66:32400/ether/me" {
		t.Fatalf("untrusted proxy portal: got %q err %v", got, err)
	}
}

func TestPortalAndSurfacePrefersConfiguredURL(t *testing.T) {
	_, bridgeNet, _ := net.ParseCIDR("172.20.0.0/16")
	containerInterfaces := []remote.InterfaceAddr{{IP: net.ParseIP("172.20.0.2"), Net: bridgeNet}}
	peer := netip.MustParseAddr("192.168.68.10")

	req := httptest.NewRequest(http.MethodPost, "http://192.168.68.66:32400/ether/api/me/apps/jellyfin", nil)
	req.RemoteAddr = "172.20.0.1:54321"

	cfg := config.Config{Plex: config.Plex{CustomURLs: []string{"https://watch.example.com:8443"}}}
	got, err := portalAddress(context.Background(), req, peer, "0.0.0.0", "32400", cfg, nil, containerInterfaces, nil)
	if err != nil || got != "https://watch.example.com:8443/ether/me" {
		t.Fatalf("custom URL portal: got %q err %v", got, err)
	}

	cfgPublic := config.Config{Plex: config.Plex{PublicHost: "media.example.com", PublicPort: 443}}
	got, err = portalAddress(context.Background(), req, peer, "0.0.0.0", "32400", cfgPublic, nil, containerInterfaces, nil)
	if err != nil || got != "https://media.example.com/ether/me" {
		t.Fatalf("public host portal: got %q err %v", got, err)
	}
}

func TestSecurePortalPrefersReachableLocalPlexDirect(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.1.0/24")
	interfaces := []remote.InterfaceAddr{{IP: net.ParseIP("192.168.1.5"), Net: network}}
	connections := []remote.Connection{
		{URI: "https://8.8.8.8:443"},
		{URI: "https://192-168-1-5.hash.plex.direct:32400", Local: true},
	}
	got, err := portalSecureAddress(context.Background(), netip.MustParseAddr("192.168.1.9"), "0.0.0.0", "32400", interfaces, connections)
	if err != nil || got != "https://192-168-1-5.hash.plex.direct:32400/ether/me" {
		t.Fatalf("local: %q %v", got, err)
	}
	got, err = portalSecureAddress(context.Background(), netip.MustParseAddr("192.168.2.9"), "0.0.0.0", "32400", interfaces, connections)
	if err != nil || got != "https://8.8.8.8:443/ether/me" {
		t.Fatalf("remote: %q %v", got, err)
	}
	if _, err = portalSecureAddress(context.Background(), netip.MustParseAddr("192.168.1.9"), "0.0.0.0", "32400", interfaces, []remote.Connection{{URI: "http://192.168.1.5:32400", Local: true}}); err == nil {
		t.Fatal("accepted insecure address")
	}
	got, err = portalSecureAddress(context.Background(), netip.MustParseAddr("192.168.1.9"), "192.168.1.6", "32400", interfaces, connections)
	if err != nil || got != "https://8.8.8.8:443/ether/me" {
		t.Fatalf("unbound local: %q %v", got, err)
	}
}

func TestSecurePortalLoopbackUsesServerLANPlexDirect(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.68.0/24")
	_, other, _ := net.ParseCIDR("10.0.0.0/24")
	interfaces := []remote.InterfaceAddr{
		{IP: net.ParseIP("192.168.68.65"), Net: network},
		{IP: net.ParseIP("10.0.0.4"), Net: other},
	}
	connections := []remote.Connection{
		{URI: "https://8.8.8.8:443"},
		{URI: "http://192.168.68.65:32400", Local: true},
		{URI: "https://10-0-0-4.hash.plex.direct:32400", Local: true},
		{URI: "https://192-168-68-65.hash.plex.direct:32400", Local: true},
	}
	want := "https://192-168-68-65.hash.plex.direct:32400/ether/me"
	for _, peer := range []string{"127.0.0.1", "::1"} {
		got, err := portalSecureAddress(context.Background(), netip.MustParseAddr(peer), "0.0.0.0", "32400", interfaces, connections)
		if err != nil || got != want {
			t.Fatalf("peer %s: %q %v", peer, got, err)
		}
	}
	got, err := portalSecureAddress(context.Background(), netip.MustParseAddr("127.0.0.1"), "10.0.0.4", "32400", interfaces, connections)
	if err != nil || got != "https://10-0-0-4.hash.plex.direct:32400/ether/me" {
		t.Fatalf("bound interface: %q %v", got, err)
	}
	localOnly := []remote.Connection{{URI: "https://192-168-68-65.hash.plex.direct:32400", Local: true}}
	if _, err = portalSecureAddress(context.Background(), netip.MustParseAddr("127.0.0.1"), "127.0.0.1", "32400", interfaces, localOnly); err == nil {
		t.Fatal("loopback-only listener advertised a LAN certificate")
	}
	if _, err = portalSecureAddress(context.Background(), netip.MustParseAddr("127.0.0.1"), "0.0.0.0", "32400", interfaces, []remote.Connection{{URI: "http://192.168.68.65:32400", Local: true}}); err == nil {
		t.Fatal("accepted insecure address for loopback")
	}
}

func TestSurfaceLANAddressUsesActualBoundPort(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.1.0/24")
	interfaces := []remote.InterfaceAddr{{IP: net.ParseIP("192.168.1.5"), Net: network}}
	peer := netip.MustParseAddr("192.168.1.9")
	local := &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 8097}
	got, ok := surfaceLANAddress(peer, local, []string{"0.0.0.0:8097"}, []string{""}, interfaces)
	if !ok || got != "http://192.168.1.5:8097" {
		t.Fatalf("dedicated port: %q %v", got, ok)
	}
	if _, ok = surfaceLANAddress(peer, &net.TCPAddr{IP: local.IP, Port: 32400}, []string{"0.0.0.0:8097"}, []string{""}, interfaces); ok {
		t.Fatal("accepted different listener")
	}
	if _, ok = surfaceLANAddress(netip.MustParseAddr("8.8.8.8"), local, []string{"0.0.0.0:8097"}, []string{""}, interfaces); ok {
		t.Fatal("exposed LAN URL to remote peer")
	}
}

func TestSurfaceSharedAddressPreservesPublishedPrefix(t *testing.T) {
	got, err := surfaceSharedAddress("https://ether.example/base/ether/me", []string{"/jellyfin"})
	if err != nil || got != "https://ether.example/base/jellyfin" {
		t.Fatalf("shared address: %q %v", got, err)
	}
	if _, err := surfaceSharedAddress("https://ether.example/base/ether/me", nil); err == nil {
		t.Fatal("missing shared mount accepted")
	}
	if _, err := surfaceSharedAddress("https://ether.example/base", []string{"/jellyfin"}); err == nil {
		t.Fatal("invalid portal path accepted")
	}
}

func TestPortalLoopbackUsesVerifiedListener(t *testing.T) {
	for _, tc := range []struct{ peer, host, want string }{
		{"127.0.0.1", "127.0.0.1", "http://127.0.0.1:1234/ether/me"},
		{"127.0.0.1", "0.0.0.0", "http://127.0.0.1:1234/ether/me"},
		{"::1", "::1", "http://[::1]:1234/ether/me"},
		{"127.0.0.1", "192.168.1.5", ""},
		{"192.168.1.5", "127.0.0.1", ""},
		{"198.51.100.2", "0.0.0.0", ""},
		{"192.168.1.5", "0.0.0.0", ""},
		{"2001:db8::5", "::", ""},
	} {
		got, err := portalLANAddress(netip.MustParseAddr(tc.peer), tc.host, "1234", nil)
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Fatalf("peer=%s bound=%s got=%q err=%v", tc.peer, tc.host, got, err)
		}
	}
}
