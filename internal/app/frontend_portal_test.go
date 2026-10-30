package app

import (
	"context"
	"net"
	"net/netip"
	"testing"

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
