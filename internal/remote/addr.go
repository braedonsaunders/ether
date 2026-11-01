// Package remote handles Ether's network presence: LAN address enumeration,
// WAN IP detection, Plex connection URIs (plain IP and *.plex.direct), UPnP
// IGD and NAT-PMP port mapping, GDM LAN discovery and reachability probes.
//
// It has no dependency on plex.tv; internal/plextv drives it.
package remote

import (
	"net"
	"sort"
	"strings"
)

// virtualPrefixes are interface name prefixes of container bridges, VPN
// tunnels and hypervisor adapters. Their addresses are not reachable by Plex
// clients on the LAN, so publishing them only makes clients waste time.
var virtualPrefixes = []string{
	"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "vnic", "cni", "flannel",
	"cali", "weave", "kube", "lxc", "lxd", "podman", "tun", "tap", "utun", "wg",
	"zt", "tailscale", "ipsec", "ppp", "gif", "stf", "awdl", "llw", "anpi", "bridge",
	"vEthernet", "Hyper-V", "VirtualBox", "VMware", "ZeroTier", "Tailscale",
}

// IsVirtualInterface reports whether an interface name looks like a
// container bridge, VPN tunnel or hypervisor adapter.
func IsVirtualInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// LANOptions tunes LAN address enumeration.
type LANOptions struct {
	// IncludeIPv6 adds global/ULA IPv6 addresses. PMS publishes IPv4 LAN
	// addresses by default; plex.tv only returns IPv6 connections when the
	// client asks with includeIPv6=1.
	IncludeIPv6 bool
	// IncludeVirtual keeps docker/VPN/hypervisor interfaces.
	IncludeVirtual bool
}

// InterfaceAddr is one usable LAN address.
type InterfaceAddr struct {
	Interface string
	IP        net.IP
	Net       *net.IPNet
}

// interfaceLister is swapped in tests.
var interfaceLister = defaultInterfaces

type ifaceInfo struct {
	name  string
	flags net.Flags
	addrs []net.Addr
}

func defaultInterfaces() ([]ifaceInfo, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceInfo, 0, len(ifs))
	for _, i := range ifs {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		out = append(out, ifaceInfo{name: i.Name, flags: i.Flags, addrs: addrs})
	}
	return out, nil
}

// LANAddresses lists the addresses Plex clients on the local network can
// reach this machine on: up, non-loopback, non-link-local, non-virtual
// interfaces. Private IPv4 addresses sort first.
func LANAddresses(opts LANOptions) ([]InterfaceAddr, error) {
	ifs, err := interfaceLister()
	if err != nil {
		return nil, err
	}
	return filterAddrs(ifs, opts), nil
}

// LANIPs is LANAddresses reduced to the IPs.
func LANIPs(opts LANOptions) ([]net.IP, error) {
	addrs, err := LANAddresses(opts)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, len(addrs))
	for i, a := range addrs {
		out[i] = a.IP
	}
	return out, nil
}

func filterAddrs(ifs []ifaceInfo, opts LANOptions) []InterfaceAddr {
	var out []InterfaceAddr
	seen := map[string]bool{}
	for _, i := range ifs {
		if i.flags&net.FlagUp == 0 || i.flags&net.FlagLoopback != 0 {
			continue
		}
		if !opts.IncludeVirtual && IsVirtualInterface(i.name) {
			continue
		}
		for _, a := range i.addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if !UsableLANIP(ip) {
				continue
			}
			if ip.To4() == nil && !opts.IncludeIPv6 {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			if seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			out = append(out, InterfaceAddr{Interface: i.name, IP: ip, Net: ipnet})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return rankIP(out[a].IP) < rankIP(out[b].IP) })
	return out
}

// IsCGNAT reports whether ip is in the 100.64.0.0/10 Shared Address Space
// (RFC 6598, also used by Tailscale). plex.tv refuses device updates that
// include a 100.64.0.0/10 connection URI.
func IsCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64
}

// UsableLANIP reports whether ip can be published as a LAN connection.
func UsableLANIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 (CGNAT, also Tailscale) and 198.18.0.0/15 (benchmarking).
		if IsCGNAT(v4) {
			return false
		}
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return false
		}
	}
	return true
}

func rankIP(ip net.IP) int {
	switch {
	case ip.To4() != nil && ip.IsPrivate():
		return 0
	case ip.To4() != nil:
		return 1
	case ip.IsPrivate():
		return 2
	default:
		return 3
	}
}

// IsPublicIP reports whether ip is a globally routable unicast address.
func IsPublicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && UsableLANIP(ip)
}
