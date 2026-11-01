package admin

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/remote"
)

// dockerBridge is the range Docker uses for bridge networks (172.16.0.0/12).
var dockerBridge = netip.MustParsePrefix("172.16.0.0/12")

// sharedPrefix is the path a protocol also serves on Ether's own port.
func sharedPrefix(id string) string {
	switch id {
	case "jellyfin", "emby", "silo", "stremio", "webdav", "m3u", "dlna":
		return "/" + id
	default:
		return ""
	}
}

// consoleConnectURLs is what the owner console shows for an app. It prefers
// the address the owner is already using (or a configured custom URL) and
// never offers a container bridge address.
func consoleConnectURLs(r *http.Request, id string, urls []string, cfg config.Config, trusted []netip.Prefix) []string {
	scheme, host, ok := requestOrigin(r, trusted)
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, raw)
	}
	prefix := sharedPrefix(id)
	if advScheme, advHost, advOK := advertisedOrigin(cfg); advOK && prefix != "" {
		add(originURL(advScheme, advHost, prefix))
	}
	// Plex is the process the owner already opened. A bridge container's
	// own address is not "this computer only".
	if id == "plex" && ok {
		add(originURL(scheme, host, ""))
	}
	if ok && prefix != "" {
		add(originURL(scheme, host, prefix))
	}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		if containerHost(u.Hostname()) && !sameHost(u.Host, host) {
			path := u.EscapedPath()
			if ok && path != "" && path != "/" {
				add(originURL(scheme, host, path))
			}
			continue
		}
		add(raw)
	}
	return rankConnectURLs(out, host)
}

// rankConnectURLs puts the address the owner can hand to a device first.
// A hostname or the LAN the console is already using wins. Home LAN
// (192.168.0.0/16) beats other private ranges, which are often VPNs.
// Loopback stays available but is not the default when a better address exists.
func rankConnectURLs(urls []string, originHost string) []string {
	type scored struct {
		url   string
		score int
		index int
	}
	rows := make([]scored, len(urls))
	for i, raw := range urls {
		rows[i] = scored{url: raw, score: connectScore(raw, originHost), index: i}
	}
	for i := 1; i < len(rows); i++ {
		row := rows[i]
		j := i
		for j > 0 && (rows[j-1].score < row.score || (rows[j-1].score == row.score && rows[j-1].index > row.index)) {
			rows[j] = rows[j-1]
			j--
		}
		rows[j] = row
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.url
	}
	return out
}

var tailscaleNet = netip.MustParsePrefix("100.64.0.0/10")

func connectScore(raw, originHost string) int {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return -1
	}
	host := hostOnly(u.Hostname())
	score := 15
	ip, ipErr := netip.ParseAddr(host)
	if ipErr != nil {
		if strings.EqualFold(host, "localhost") {
			score = 0
		} else {
			score = 70
		}
	} else {
		ip = ip.Unmap()
		switch {
		case ip.IsLoopback():
			score = 0
		case ip.Is4() && homeLAN(ip):
			score = 80
		case tailscaleNet.Contains(ip):
			score = 20
		case ip.IsPrivate():
			score = 40
		case ip.Is6():
			score = 10
		}
	}
	if originHost != "" && sameHost(host, originHost) && score >= 40 {
		score += 25
	}
	// Prefer the port the console is already open on, so the default matches
	// the address the owner is looking at.
	if originPort(originHost) != "" && originPort(originHost) == u.Port() && score >= 40 {
		score += 5
	}
	return score
}

func originPort(hostport string) string {
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return ""
	}
	return port
}

func homeLAN(ip netip.Addr) bool {
	b := ip.As4()
	return b[0] == 192 && b[1] == 168
}

func advertisedOrigin(cfg config.Config) (scheme, host string, ok bool) {
	for _, raw := range cfg.Plex.CustomURLs {
		if _, err := remote.ParseCustomURL(raw); err != nil {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || containerHost(u.Hostname()) {
			continue
		}
		return u.Scheme, u.Host, true
	}
	return "", "", false
}

func requestOrigin(r *http.Request, trusted []netip.Prefix) (scheme, host string, ok bool) {
	host = r.Host
	scheme = "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proxyTrusted(r, trusted) {
		if forwarded := forwardedHost(r); forwarded != "" && !containerHost(hostOnly(forwarded)) {
			host = forwarded
		}
		if proto := forwardedProto(r); proto == "https" || proto == "http" {
			scheme = proto
		}
	}
	if host == "" || containerHost(hostOnly(host)) {
		return "", "", false
	}
	return scheme, host, true
}

func proxyTrusted(r *http.Request, trusted []netip.Prefix) bool {
	peer, err := netip.ParseAddr(hostOnly(r.RemoteAddr))
	if err != nil {
		return false
	}
	peer = peer.Unmap()
	if peer.IsLoopback() {
		return true
	}
	for _, p := range trusted {
		if p.Contains(peer) {
			return true
		}
	}
	return false
}

func forwardedHost(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); h != "" {
		return sanitizeHost(strings.TrimSpace(strings.Split(h, ",")[0]))
	}
	return ""
}

func forwardedProto(r *http.Request) string {
	p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	return strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
}

func sanitizeHost(host string) string {
	if host == "" || strings.ContainsAny(host, " \t\r\n/@") || len(host) > 255 {
		return ""
	}
	return host
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(hostport, "[]")
}

func sameHost(a, b string) bool {
	return strings.EqualFold(hostOnly(a), hostOnly(b))
}

func containerHost(host string) bool {
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	return dockerBridge.Contains(ip.Unmap())
}

func originURL(scheme, host, path string) string {
	u := &url.URL{Scheme: scheme, Host: host}
	if path != "" {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		u.Path = path
	}
	return u.String()
}
