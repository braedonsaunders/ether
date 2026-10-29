package frontend

import (
	"net/netip"
	"testing"
)

func TestLANDeviceUsesCanonicalPeer(t *testing.T) {
	a := LANDevice(netip.MustParseAddr("192.0.2.1"))
	b := LANDevice(netip.MustParseAddr("::ffff:192.0.2.1"))
	if a.ID == "" || a.ID != b.ID || a.ID == LANDevice(netip.MustParseAddr("192.0.2.2")).ID || LANDevice(netip.Addr{}).ID != "" {
		t.Fatalf("devices: %+v %+v", a, b)
	}
}
