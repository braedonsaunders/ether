package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/remote"
)

// viewerPortalURL uses the verified client address and the main listener's
// published network addresses; it never trusts request Host or forwarded host.
func (r *frontendRuntime) viewerPortalURL(ctx context.Context, req *http.Request) (string, error) {
	ip, ok := r.app.frontendClientIP(req)
	if !ok {
		return "", errors.New("client address unavailable")
	}
	r.mu.Lock()
	addr := r.mainAddr
	r.mu.Unlock()
	if addr == nil {
		return "", errors.New("main listener unavailable")
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", err
	}
	if identity.LAN(ip) {
		interfaces, err := remote.LANAddresses(remote.LANOptions{IncludeIPv6: true})
		if err != nil {
			return "", err
		}
		return portalLANAddress(ip, host, port, interfaces)

	}
	var connections []remote.Connection
	for _, raw := range r.app.cfg.Get().Plex.CustomURLs {
		connections = append(connections, remote.Connection{URI: raw, Custom: true})
	}
	if r.app.registrar != nil {
		connections = append(connections, r.app.registrar.Status().Connections...)
	}
	return portalPublicAddress(ctx, connections)
}

func portalLANAddress(ip netip.Addr, host, port string, interfaces []remote.InterfaceAddr) (string, error) {
	for _, candidate := range interfaces {
		if candidate.Net == nil || !candidate.Net.Contains(net.IP(ip.AsSlice())) {
			continue
		}
		bound := net.ParseIP(host)
		if bound != nil && !bound.IsUnspecified() && !bound.Equal(candidate.IP) {
			continue
		}
		u := url.URL{Scheme: "http", Host: net.JoinHostPort(candidate.IP.String(), port), Path: "/ether/me"}
		return u.String(), nil
	}
	return "", errors.New("no reachable LAN address")
}

func portalPublicAddress(ctx context.Context, connections []remote.Connection) (string, error) {
	for _, c := range connections {
		if c.Local || c.Relay {
			continue
		}
		u, err := url.Parse(c.URI)
		if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
			continue
		}
		hostname := u.Hostname()
		if p := net.ParseIP(hostname); p != nil {
			if !remote.IsPublicIP(p) {
				continue
			}
		} else if p, _, ok := remote.ParsePlexDirectHost(hostname); ok {
			if !remote.IsPublicIP(p) {
				continue
			}
		} else {
			ips, e := net.DefaultResolver.LookupIPAddr(ctx, hostname)
			if e != nil || len(ips) == 0 {
				continue
			}
			valid := true
			for _, a := range ips {
				if !remote.IsPublicIP(a.IP) {
					valid = false
					break
				}
			}
			if !valid {
				continue
			}
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/ether/me"
		u.RawPath = ""
		return u.String(), nil
	}
	return "", errors.New("public address unavailable")
}
