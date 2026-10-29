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
//
// Two further invariants keep the dial-time check from being bypassed:
//
//   - No proxy. Guarded transports always dial the destination directly
//     (Proxy is forced to nil, so neither a proxy configured on the base
//     transport nor HTTP_PROXY/HTTPS_PROXY in the environment is ever
//     used). A proxy would be dialed instead of the destination, so the
//     guard would validate the proxy's address and the proxy would then
//     happily fetch a private target. Fetching indexers and definition
//     sources therefore deliberately ignores proxy configuration.
//   - Separate connection pools per policy. A guarded client carries
//     two transports with independent pools: one only ever dials the
//     exact trusted origin, the other only ever dials public addresses.
//     Each request (and each redirect hop) is routed to one pool by its
//     own destination, so a connection opened to a private trusted
//     origin can never be reused by a later untrusted request to the
//     same host — reuse would skip the dial (and its check) entirely.
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

// originKey returns the host:port key for u, filling in the scheme's
// default port. It identifies an origin exactly as WithTrustedOrigin
// records it.
func originKey(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return hostPortKey(u.Hostname(), port)
}

// SameOrigin reports whether two URLs share exactly the same host:port
// (with default ports filled in). It is how callers decide whether a
// fetch targets the owner-configured origin at all.
func SameOrigin(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	if errA != nil || errB != nil || ua.Hostname() == "" || ub.Hostname() == "" {
		return false
	}
	return originKey(ua) == originKey(ub)
}

// WithTrustedOriginFor marks trusted as the trusted origin only when
// rawURL actually targets that exact origin; any other rawURL gets an
// explicitly untrusted (public-only) context. Callers select the policy
// per fetch with this instead of marking every request trusted and
// relying on the dialer to sort it out: a request for a feed-supplied
// URL on another host is public from the start, and a trusted client
// can never be aimed at a different host.
func WithTrustedOriginFor(ctx context.Context, trusted, rawURL string) context.Context {
	if SameOrigin(trusted, rawURL) {
		return WithTrustedOrigin(ctx, trusted)
	}
	// Clear any trust an outer context may carry: this fetch is public.
	return context.WithValue(ctx, trustedOriginKey{}, "")
}

// isTrustedRequest reports whether req's destination is exactly the
// trusted origin marked in its context.
func isTrustedRequest(req *http.Request) bool {
	trusted := trustedOrigin(req.Context())
	return trusted != "" && trusted == originKey(req.URL)
}

// DialContext resolves the host in addr, refuses non-public addresses
// (unless addr is exactly the context's trusted origin) and dials a
// validated address. Because the check happens here, it applies to the
// initial request and to every redirect hop alike.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return g.dialChecked(ctx, network, addr, false)
}

// publicDialContext is the public pool's dialer: it refuses non-public
// addresses unconditionally, even if the request context marks a
// trusted origin — the public pool must never open (or lend) a
// connection to a private address.
func (g *Guard) publicDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return g.dialChecked(ctx, network, addr, false)
}

// trustedDialContext is the trusted pool's dialer: it only dials addr
// when addr is exactly the context's trusted origin, and refuses
// everything else outright (the routing in transport.RoundTrip should
// already guarantee this; the dialer re-checks as defence in depth).
func (g *Guard) trustedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return g.dialChecked(ctx, network, addr, true)
}

func (g *Guard) dialChecked(ctx context.Context, network, addr string, trustedPool bool) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	trusted := trustedOrigin(ctx) == hostPortKey(host, port)
	if trustedPool && !trusted {
		return nil, fmt.Errorf("%w: %s is not the trusted origin", ErrBlocked, addr)
	}
	if !trustedPool {
		// The public pool ignores any trusted origin in the context.
		trusted = false
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
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

// transport routes each request to one of two transports with
// separate connection pools, chosen by the request's own policy:
// requests to exactly the context's trusted origin go to the trusted
// pool (which may dial private addresses, that origin only); every
// other request goes to the public pool (public addresses only).
// Because the pools never share connections, a connection opened to a
// private trusted origin cannot be reused by an untrusted request —
// reuse would skip the dial and its address check. The routing runs
// per RoundTrip, so each redirect hop is routed by its own destination.
type transport struct {
	public  http.RoundTripper
	trusted http.RoundTripper
	guard   *Guard
}

// RoundTrip refuses non-http(s) requests outright and routes the rest
// by policy; address validation happens in the chosen pool's guarded
// dialer.
func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Hostname() == "" {
		return nil, fmt.Errorf("%w: %q is not an http(s) URL with a host", ErrBlocked, req.URL.Redacted())
	}
	if isTrustedRequest(req) {
		return t.trusted.RoundTrip(req)
	}
	return t.public.RoundTrip(req)
}

// policyTransport builds one pool's transport from base. A
// *http.Transport base is cloned — Clone gives the clone its own
// connection pool — with Proxy forced to nil (guarded fetches never
// use a proxy, configured or from the environment: the guard must dial
// and validate the destination itself, not hand the request to a
// proxy that would fetch an unchecked target) and with the dial hooks
// replaced by the pool's guarded dialer. Dial/DialTLS/DialTLSContext,
// when set, would bypass DialContext, so they are dropped (the
// transport then performs any TLS handshake over the guarded dial).
func (g *Guard) policyTransport(base http.RoundTripper, trusted bool) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	dial := g.publicDialContext
	if trusted {
		dial = g.trustedDialContext
	}
	if ht, ok := base.(*http.Transport); ok {
		clone := ht.Clone()
		clone.Proxy = nil
		clone.DialContext = dial
		clone.Dial = nil
		clone.DialTLSContext = nil
		clone.DialTLS = nil
		clone.OnProxyConnectResponse = nil
		return clone
	}
	// A custom RoundTripper has no dialer or pool the guard can split
	// or reconfigure; validate the destination before handing over
	// instead (trusted: exact origin only; public: resolves public).
	return policyRoundTripper{base: base, guard: g, trusted: trusted}
}

type policyRoundTripper struct {
	base    http.RoundTripper
	guard   *Guard
	trusted bool
}

func (p policyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if p.trusted {
		if !isTrustedRequest(req) {
			return nil, fmt.Errorf("%w: %q is not the trusted origin", ErrBlocked, req.URL.Redacted())
		}
		return p.base.RoundTrip(req)
	}
	if err := p.guard.ValidatePublicURL(req.Context(), req.URL.String()); err != nil {
		return nil, err
	}
	return p.base.RoundTrip(req)
}

// Transport returns a RoundTripper that validates destinations at dial
// time, with separate trusted-origin and public connection pools and
// no proxy (see the package documentation). A nil base means
// http.DefaultTransport.
func (g *Guard) Transport(base http.RoundTripper) http.RoundTripper {
	if g == nil {
		g = &Guard{}
	}
	return transport{
		public:  g.policyTransport(base, false),
		trusted: g.policyTransport(base, true),
		guard:   g,
	}
}

// Client returns a copy of base whose transport and redirect policy are
// guarded. The caller selects the policy per request: the
// owner-configured origin via WithTrustedOrigin/WithTrustedOriginFor
// on the request context (trusted pool), anything else without it
// (public pool).
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
