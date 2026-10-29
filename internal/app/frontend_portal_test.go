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
