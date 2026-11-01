package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// Outbound is this computer's optional proxy for source traffic.
// It is node-local and off by default. While it is off, Ether dials the
// way it always has. Passwords are sealed with the other secrets; the URL
// must not carry them.
type Outbound struct {
	Enabled bool `json:"enabled"`
	// Preset is "gluetun", "custom", or empty. It only affects the owner
	// console; routing uses URL.
	Preset   string `json:"preset,omitempty"`
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Source routes default on. Metadata stays off until the owner opts in.
	// Plex, updates, UPnP and remote access are never routed here.
	Indexers bool `json:"indexers"`
	Stremio  bool `json:"stremio"`
	Debrid   bool `json:"debrid"`
	Usenet   bool `json:"usenet"`
	Metadata bool `json:"metadata"`
	// Bypass are extra hostnames that stay on the normal route.
	// Loopback and literal private addresses always do.
	Bypass []string `json:"bypass,omitempty"`
}

// OutboundProblem is one owner-fixable problem with an outbound proxy.
type OutboundProblem struct {
	Field   string
	Message string
}

var errOutboundURL = errors.New("outbound proxy address is not valid")

// Problems reports validation errors. A disabled proxy may keep a blank
// address. A non-empty address is checked either way, and an enabled proxy
// requires one. Messages never include the address or the password.
func (o Outbound) Problems() []OutboundProblem {
	var out []OutboundProblem
	raw := strings.TrimSpace(o.URL)
	switch o.Preset {
	case "", "gluetun", "custom":
	default:
		out = append(out, OutboundProblem{Field: "outbound.preset", Message: "must be a VPN container or a custom address"})
	}
	if strings.TrimSpace(o.Username) != "" && !plainSecret(o.Username, false) {
		out = append(out, OutboundProblem{Field: "outbound.username", Message: "cannot contain spaces or control characters"})
	}
	if o.Password != "" && !plainSecret(o.Password, true) {
		out = append(out, OutboundProblem{Field: "outbound.password", Message: "cannot contain control characters"})
	}
	if raw == "" {
		if o.Enabled {
			out = append(out, OutboundProblem{Field: "outbound.url", Message: "is required when the proxy is on"})
		}
	} else if _, err := parseProxyURL(raw); err != nil {
		out = append(out, OutboundProblem{Field: "outbound.url", Message: "must be an http://, https://, or socks5:// address with a host and port"})
	}
	if len(o.Bypass) > 32 {
		out = append(out, OutboundProblem{Field: "outbound.bypass", Message: "can list at most 32 names"})
	}
	for _, b := range o.Bypass {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		if !validBypass(b) {
			out = append(out, OutboundProblem{Field: "outbound.bypass", Message: "each entry is a hostname or IP address"})
			break
		}
	}
	return out
}

// ProxyURL is the proxy Ether dials, with username and password attached
// when they are set. The error is fixed text and never contains the URL.
func (o Outbound) ProxyURL() (*url.URL, error) {
	return parseProxyURL(strings.TrimSpace(o.URL))
}

func parseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return nil, errOutboundURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errOutboundURL
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errOutboundURL
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return nil, errOutboundURL
	}
	port := u.Port()
	n, nerr := strconv.Atoi(port)
	if port == "" || nerr != nil || n < 1 || n > 65535 {
		return nil, errOutboundURL
	}
	u.Path = ""
	return u, nil
}

func plainSecret(s string, allowSpace bool) bool {
	if len(s) > 256 {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
		if !allowSpace && unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validBypass(s string) bool {
	if len(s) > 253 || strings.Contains(s, "://") || strings.ContainsAny(s, " /?#@") {
		return false
	}
	host := s
	if h, _, err := net.SplitHostPort(s); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, ".")
	if host == "" || strings.Contains(host, "..") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
