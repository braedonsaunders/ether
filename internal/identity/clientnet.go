package identity

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

var overlay = netip.MustParsePrefix("100.64.0.0/10")

// LAN reports whether an address belongs to a LAN or overlay network.
func LAN(ip netip.Addr) bool {
	return ip.IsValid() && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || overlay.Contains(ip.Unmap()))
}

// ClientNet classifies the peer and returns its client address. Forwarding
// headers are trusted only from loopback or explicitly configured proxies.
// Malformed chains fail closed. A public hop cannot be hidden by a preceding
// client-supplied private address.
func ClientNet(r *http.Request, trusted ...netip.Prefix) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	peer = peer.Unmap()
	trust := peer.IsLoopback()
	for _, p := range trusted {
		if p.Contains(peer) {
			trust = true
		}
	}
	if !trust {
		return peer, LAN(peer)
	}
	var hops []string
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		hops = strings.Split(strings.Join(values, ","), ",")
	} else if values := r.Header.Values("Forwarded"); len(values) > 0 {
		for _, element := range strings.Split(strings.Join(values, ","), ",") {
			found := false
			for _, param := range strings.Split(element, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(k, "for") {
					if found {
						return netip.Addr{}, false
					}
					found = true
					v = strings.Trim(strings.TrimSpace(v), "\"")
					if h, _, e := net.SplitHostPort(v); e == nil {
						v = h
					}
					hops = append(hops, strings.Trim(v, "[]"))
				}
			}
			if !found {
				return netip.Addr{}, false
			}
		}
	} else if v := r.Header.Get("X-Real-IP"); v != "" {
		hops = []string{v}
	}
	if len(hops) == 0 {
		return peer, LAN(peer)
	}
	var nearest netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		ip, e := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if e != nil {
			return netip.Addr{}, false
		}
		ip = ip.Unmap()
		if !LAN(ip) {
			proxy := false
			for _, p := range trusted {
				if p.Contains(ip) {
					proxy = true
					break
				}
			}
			if !proxy {
				return ip, false
			}
			continue
		}
		if !nearest.IsValid() {
			nearest = ip
		}
	}
	return nearest, LAN(nearest)
}

// ProxyPrefixes parses configured peer networks. Invalid entries grant no trust.
func ProxyPrefixes(cidrs []string) []netip.Prefix {
	var out []netip.Prefix
	for _, raw := range cidrs {
		if p, err := netip.ParsePrefix(strings.TrimSpace(raw)); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}
