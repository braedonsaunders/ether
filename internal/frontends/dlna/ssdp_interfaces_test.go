package dlna

import (
	"errors"
	"net"
	"testing"
)

func TestSSDPMultiInterfaceMembershipRefresh(t *testing.T) {
	up := net.FlagUp | net.FlagMulticast
	interfaces := []net.Interface{
		{Index: 1, Name: "lo", Flags: up | net.FlagLoopback},
		{Index: 2, Name: "lan", Flags: up},
		{Index: 3, Name: "docker", Flags: up},
		{Index: 4, Name: "broken-vpn", Flags: up},
		{Index: 5, Name: "down", Flags: net.FlagMulticast},
	}
	joined := map[int]net.Interface{}
	attempts := map[int]int{}
	var left []int
	join := func(iface *net.Interface) error {
		attempts[iface.Index]++
		if iface.Index == 4 {
			return errors.New("join refused")
		}
		return nil
	}
	leave := func(iface *net.Interface) error { left = append(left, iface.Index); return nil }
	reconcileSSDPInterfaces(interfaces, joined, join, leave)
	if len(joined) != 2 || joined[2].Name != "lan" || joined[3].Name != "docker" || attempts[4] != 1 || attempts[1] != 0 || attempts[5] != 0 {
		t.Fatalf("joined=%v attempted=%v", joined, attempts)
	}
	// LAN remains joined; a vanished bridge is withdrawn and a new NIC joins.
	next := []net.Interface{interfaces[1], interfaces[3], {Index: 6, Name: "wifi", Flags: up}}
	reconcileSSDPInterfaces(next, joined, join, leave)
	if len(joined) != 2 || joined[6].Name != "wifi" || attempts[2] != 1 || attempts[4] != 2 || len(left) != 1 || left[0] != 3 {
		t.Fatalf("joined=%v attempted=%v left=%v", joined, attempts, left)
	}
	reconcileSSDPInterfaces(nil, joined, join, leave)
	if len(joined) != 0 || len(left) != 3 {
		t.Fatalf("joined=%v left=%v", joined, left)
	}
}
