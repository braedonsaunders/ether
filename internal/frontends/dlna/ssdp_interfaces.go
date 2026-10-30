package dlna

import (
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
)

func multicastInterface(iface net.Interface) bool {
	return iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagMulticast != 0 && iface.Flags&net.FlagLoopback == 0
}

func listenSSDP(group *net.UDPAddr) (*net.UDPConn, *ipv4.PacketConn, map[int]net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, nil, err
	}
	var conn *net.UDPConn
	joined := make(map[int]net.Interface)
	for _, iface := range interfaces {
		if !multicastInterface(iface) {
			continue
		}
		conn, err = net.ListenMulticastUDP("udp4", &iface, group)
		if err == nil {
			joined[iface.Index] = iface
			break
		}
	}
	if conn == nil {
		return nil, nil, nil, fmt.Errorf("no usable IPv4 multicast interface: %v", err)
	}
	packet := ipv4.NewPacketConn(conn)
	if err = packet.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	reconcileSSDPInterfaces(interfaces, joined, func(iface *net.Interface) error { return packet.JoinGroup(iface, group) }, func(iface *net.Interface) error { return packet.LeaveGroup(iface, group) })
	return conn, packet, joined, nil
}

// Keep working memberships when a VPN or another interface cannot join.
func reconcileSSDPInterfaces(interfaces []net.Interface, joined map[int]net.Interface, join, leave func(*net.Interface) error) {
	current := make(map[int]net.Interface)
	for _, iface := range interfaces {
		if multicastInterface(iface) {
			current[iface.Index] = iface
		}
	}
	for index, iface := range joined {
		if next, ok := current[index]; !ok || next.Name != iface.Name {
			_ = leave(&iface)
			delete(joined, index)
		}
	}
	for index, iface := range current {
		if _, ok := joined[index]; ok {
			continue
		}
		if join(&iface) == nil {
			joined[index] = iface
		}
	}
}

func (s *ssdp) refreshInterfaces() {
	interfaces, err := net.Interfaces()
	if err != nil {
		return
	}
	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.packet == nil {
		return
	}
	reconcileSSDPInterfaces(interfaces, s.joined, func(iface *net.Interface) error { return s.packet.JoinGroup(iface, group) }, func(iface *net.Interface) error { return s.packet.LeaveGroup(iface, group) })
}
