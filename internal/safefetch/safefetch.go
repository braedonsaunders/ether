// Package safefetch guards HTTP fetches of untrusted URLs against SSRF.
//
// URLs that arrive in remote data (an indexer's feed, a definitions
// listing) can point at cloud metadata endpoints, loopback services or
// LAN hosts that the remote peer has no business reaching through Ether.
// A Guard client therefore validates the destination at dial time: the
// host is resolved, every candidate address must be a public unicast
// address, and the connection is opened to the validated address itself,
// so a DNS answer that flips between validation and dialling (rebinding)
// cannot smuggle the request to a private target. TLS still uses the
// original host name (the transport derives the ServerName and Host
// header from the URL, not from the dialed address).
//
// The one exception is the owner-configured origin: an indexer or a
// definitions source the owner deliberately pointed at a LAN host must
// keep working. Callers mark that origin with WithTrustedOrigin; dials
// to exactly that host:port are allowed whatever address it resolves to.
// Any other destination — including every redirect hop away from the
// trusted origin — must be public. Redirects are revalidated on each hop
// both by CheckRedirect (scheme/length) and by the dialer (address).
package safefetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// ErrBlocked reports a destination the guard refused: a non-http(s)
// scheme, or a host that resolves to a non-public address.
var ErrBlocked = errors.New("safefetch: blocked non-public destination")

// cgnat is RFC 6598 shared address space (100.64.0.0/10, also Tailscale).
// The repo already treats CGNAT as local when classifying Plex clients
// (internal/plex isLocalAddr), so it is not a public destination here
// either: a feed must not reach the owner's overlay network.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// IsPublicIP reports whether ip is a routable public unicast address.
// Loopback, RFC 1918 / RFC 4193 private, link-local (including
// 169.254.0.0/16 cloud metadata), CGNAT, multicast and unspecified
// addresses are not public.
func IsPublicIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		cgnat.Contains(ip) {
		return false
	}
	return ip.IsGlobalUnicast()
}

// Guard builds guarded HTTP clients. The zero value is ready to use;
// LookupIP and Dial exist so tests can map fake public host names onto
// local test servers.
type Guard struct {
	// LookupIP resolves host to addresses; nil uses the system resolver.
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial opens a connection to a validated "IP:port" address; nil uses
	// a zero net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (g *Guard) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if g != nil && g.LookupIP != nil {
		return g.LookupIP(ctx, host)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	seen := map[netip.Addr]bool{}
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok || seen[ip] {
			continue
		}
		ip = ip.Unmap()
		seen[ip] = true
		out = append(out, ip)
	}
	return out, nil
}

func (g *Guard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if g != nil && g.Dial != nil {
		return g.Dial(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// resolve returns the addresses host dials to: the literal address for
// IP hosts, a fresh lookup otherwise.
func (g *Guard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}
	ips, err := g.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: %s does not resolve", ErrBlocked, host)
	}
	return ips, nil
}

// ValidatePublicURL checks that rawURL is an http(s) URL whose host
// resolves only to public addresses. It validates at call time; fetches
// should still go through a Guard client, which revalidates at dial time.
func (g *Guard) ValidatePublicURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBlocked, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("%w: %q is not an http(s) URL with a host", ErrBlocked, rawURL)
	}
	ips, err := g.resolve(ctx, u.Hostname())
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if !IsPublicIP(ip) {
			return fmt.Errorf("%w: %s resolves to %s", ErrBlocked, u.Hostname(), ip)
		}
	}
	return nil
}

// ValidatePublicURL validates rawURL with a zero Guard.
func ValidatePublicURL(ctx context.Context, rawURL string) error {
	return (&Guard{}).ValidatePublicURL(ctx, rawURL)
}

type trustedOriginKey struct{}

// WithTrustedOrigin marks rawURL's origin (host:port, with the scheme's
// default port filled in) as owner-configured and therefore allowed to
// resolve to private addresses. Anything else fetched with the returned
// context must still be public.
func WithTrustedOrigin(ctx context.Context, rawURL string) context.Context {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return ctx
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return context.WithValue(ctx, trustedOriginKey{}, hostPortKey(u.Hostname(), port))
}

func hostPortKey(host, port string) string {
	return strings.ToLower(strings.Trim(host, "[]")) + ":" + port
}

func trustedOrigin(ctx context.Context) string {
	if v, ok := ctx.Value(trustedOriginKey{}).(string); ok {
		return v
	}
	return ""
}

// DialContext resolves the host in addr, refuses non-public addresses
// (unless addr is exactly the context's trusted origin) and dials a
// validated address. Because the check happens here, it applies to the
// initial request and to every redirect hop alike.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	trusted := trustedOrigin(ctx) == hostPortKey(host, port)
	var lastErr error
	blocked := false
	for _, ip := range ips {
		if network == "tcp4" && ip.Is6() || network == "tcp6" && ip.Is4() {
			continue
		}
		if !trusted && !IsPublicIP(ip) {
			blocked = true
			continue
		}
		conn, err := g.dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if blocked {
		return nil, fmt.Errorf("%w: %s resolves to a non-public address", ErrBlocked, host)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: %s does not resolve to a usable address", ErrBlocked, host)
}

// checkRedirect revalidates each redirect hop's scheme; the dialer
// revalidates the hop's address when the hop is actually fetched.
func (g *Guard) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("safefetch: stopped after 10 redirects")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("%w: redirect to scheme %q", ErrBlocked, req.URL.Scheme)
	}
	return nil
}

type transport struct {
	base  http.RoundTripper
	guard *Guard
}

// RoundTrip refuses non-http(s) requests outright; address validation
// happens in the base transport's guarded DialContext. A custom
// RoundTripper without a dialer cannot be address-checked, but the
// scheme check still applies.
func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Hostname() == "" {
		return nil, fmt.Errorf("%w: %q is not an http(s) URL with a host", ErrBlocked, req.URL.Redacted())
	}
	return t.base.RoundTrip(req)
}

// Transport returns a RoundTripper that validates destinations at dial
// time. A *http.Transport base is cloned with its DialContext replaced;
// any other base (including nil, meaning http.DefaultTransport) is
// wrapped as-is.
func (g *Guard) Transport(base http.RoundTripper) http.RoundTripper {
	if g == nil {
		g = &Guard{}
	}
	if base == nil {
		base = http.DefaultTransport
	}
	if ht, ok := base.(*http.Transport); ok {
		clone := ht.Clone()
		// DialTLSContext, when set, would bypass DialContext for TLS;
		// drop it so https fetches are dial-checked too (the transport
		// then performs the handshake itself over the guarded dial).
		clone.DialContext = g.DialContext
		clone.DialTLSContext = nil
		base = clone
	}
	return transport{base: base, guard: g}
}

// Client returns a copy of base whose transport and redirect policy are
// guarded. The caller marks the owner-configured origin per request
// with WithTrustedOrigin on the request context.
func (g *Guard) Client(base *http.Client) *http.Client {
	if g == nil {
		g = &Guard{}
	}
	if base == nil {
		base = &http.Client{}
	}
	c := *base
	c.Transport = g.Transport(base.Transport)
	inner := base.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := g.checkRedirect(req, via); err != nil {
			return err
		}
		if inner != nil {
			return inner(req, via)
		}
		return nil
	}
	return &c
}
