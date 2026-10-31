// Package safefetch guards HTTP fetches of untrusted URLs against SSRF.
//
// URLs that arrive in remote data (an indexer's feed, a definitions
// listing) can point at cloud metadata endpoints, loopback services or
// LAN hosts that the remote peer has no business reaching through Ether.
// A Guard client therefore binds every request to the destination it
// validated, before any transport sees it:
//
//   - O destino é resolvido e validado UMA vez por RoundTrip (cada hop
//     de redirect é um novo RoundTrip e decide de novo): todo endereço
//     candidato precisa ser público — qualquer um não-público recusa a
//     requisição — e os endereços validados são preservados para o dial direto.
//   - Direct connections use a pool key containing the original hostname
//     and validated address set. The dial hook connects only to that set,
//     and TLS uses the
//     original hostname. Proxy requests carry the IP in the absolute URI
//     or CONNECT target; their Host header and TLS name remain original.
//     HTTP proxy requests and HTTPS CONNECT tunnels use dedicated connections.
//
// The one exception is the owner-configured origin: an indexer or a
// definitions source the owner deliberately pointed at a LAN host must
// keep working. Callers mark that origin with WithTrustedOrigin; dials
// to exactly that host:port are allowed whatever address it resolves to.
// Any other destination — including every redirect hop away from the
// trusted origin — must be public. Redirects are revalidated on each hop
// both by CheckRedirect (scheme/length) and by the dialer (address).
//
// Proxy handling is explicit, never a silent bypass of the owner's
// network setup:
//
//   - A rota é selecionada UMA vez por requisição: a função Proxy do
//     transporte base (configurada ou do ambiente) é chamada uma única
//     vez, a decisão (proxy ou direto, IP validado, hostname original)
//     é guardada no contexto do clone da requisição, e a Proxy func do
//     transporte interno apenas CONSOME essa decisão — nunca chama o
//     seletor de novo. Um seletor stateful não pode mudar a rota entre
//     a validação e o uso.
//   - Sem proxy: o dial é direto, no IP já validado (pinado).
//   - Com proxy: a rota do dono é PRESERVADA, mas o destino já vai
//     reescrito para o IP validado (acima), e destino não-público é
//     recusado antes de o proxy ser chamado. O host do proxy em si pode
//     ser privado: é a origem de rede que o dono escolheu.
//   - http, https and socks5 proxies are pinned to the validated IP.
//     socks5 is not allowed to resolve the name (the address type is the
//     IP, not a domain). Any other scheme, or a proxy with no host,
//     returns ErrUnsupportedTransport. A failed proxy dial is not retried
//     as a direct connection. A destination the guard has already allowed
//     that is not public (the trusted-origin exception) stays direct, so
//     a LAN indexer is not sent to the proxy.
//
// Two further invariants keep the check from being bypassed:
//
//   - Transporte clonável apenas. Só *http.Transport pode ser guardado:
//     ele é clonado para que dial, proxy e pools sejam reconfigurados
//     com enforcement. Qualquer outro RoundTripper (custom/wrapped)
//     produz ErrUnsupportedTransport em cada requisição, em vez de
//     buscar sem pinagem/validação no dial.
//   - Separate connection pools per policy. A guarded client carries
//     two transports with independent pools: one only ever dials the
//     exact trusted origin, the other only ever dials public addresses.
//     Each request (and each redirect hop) is routed to one pool by its
//     own destination, so a connection opened to a private trusted
//     origin can never be reused by a later untrusted request to the
//     same host — reuse would skip the dial (and its check) entirely.
//     As variantes direta e via-proxy de cada política também têm pools
//     próprios (exceto os modos por-requisição declarados acima).
package safefetch

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// ErrBlocked reports a destination the guard refused: a non-http(s)
// scheme, or a host that resolves to a non-public address.
var ErrBlocked = errors.New("safefetch: blocked non-public destination")

// ErrUnsupportedTransport reports a base transport the guard cannot
// enforce its policy on: anything that is not a clonable
// *http.Transport. Guarded clients return it instead of fetching
// through a custom RoundTripper without dial-time enforcement.
var ErrUnsupportedTransport = errors.New("safefetch: unsupported transport")

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
	return g.validatePublicDestination(ctx, u)
}

// validatePublicDestination resolves u's host and refuses it when any
// candidate address is not public. It is the pre-proxy check: with a
// proxy the request still goes to the proxy afterwards, but a
// non-public destination never reaches it.
func (g *Guard) validatePublicDestination(ctx context.Context, u *url.URL) error {
	_, err := g.resolveValidatedIPs(ctx, u.Hostname(), false)
	return err
}

// resolveValidatedIPs resolves host and returns the validated addresses.
// Unless trusted (exactly the owner-configured
// origin, where a private LAN address is legitimate), EVERY candidate
// must be public: a mixed answer is refused rather than picking the
// public-looking one, so a rebinding answer cannot hide a private
// candidate behind a public one.
func (g *Guard) resolveValidatedIPs(ctx context.Context, host string, trusted bool) ([]netip.Addr, error) {
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if !trusted && !IsPublicIP(ip) {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlocked, host, ip)
		}
	}
	return ips, nil
}

// routeDecision is the route chosen ONCE for one request: which proxy
// (nil = direct), the validated IP the request is rewritten to, and
// the original host name (Host header / TLS ServerName). It travels in
// the cloned request's context; the inner transports' Proxy and TLS
// dial hooks only consume it and never re-run the owner's selector.
type routeDecision struct {
	trusted    bool
	proxy      *url.URL
	ip         netip.Addr
	ips        []netip.Addr
	host       string // original hostname (no port)
	targetAddr string // validated IP:port
	poolAddr   string // direct pool key, including hostname and address set
}

type routeDecisionKey struct{}

func decisionFromContext(ctx context.Context) (*routeDecision, bool) {
	dec, ok := ctx.Value(routeDecisionKey{}).(*routeDecision)
	return dec, ok && dec != nil
}

// proxyFromDecision is the Proxy func of the inner proxied transports.
// It returns the proxy decided once for this request (stored in its
// context) and never calls the owner's selector again — a stateful
// selector cannot change the route between validation and use. A
// proxied transport reached without a decision has no route to
// consume, so it fails instead of selecting one silently.
func proxyFromDecision(req *http.Request) (*url.URL, error) {
	dec, ok := decisionFromContext(req.Context())
	if !ok {
		return nil, fmt.Errorf("%w: proxy route without a prior route decision", ErrUnsupportedTransport)
	}
	return dec.proxy, nil
}

func cloneTLSConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return cfg.Clone()
}

// dialTLSContext builds the direct pool's DialTLSContext: it dials the
// validated IP through the guarded dialer (addr is already an IP
// literal after the rewrite, so no name is re-resolved) and wraps it
// in TLS whose ServerName is the ORIGINAL hostname from the request's
// route decision — never the shared transport's mutable config, which
// is only cloned here, per connection.
func (g *Guard) dialTLSContext(dial func(ctx context.Context, network, addr string) (net.Conn, error), base *tls.Config) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		plain, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		cfg := cloneTLSConfig(base)
		if cfg.ServerName == "" {
			name := addr
			if host, _, err := net.SplitHostPort(addr); err == nil {
				name = host
			}
			if dec, ok := decisionFromContext(ctx); ok && dec.host != "" {
				name = dec.host
			}
			cfg.ServerName = name
		}
		// The transport handshakes a returned *tls.Conn itself.
		return tls.Client(plain, cfg), nil
	}
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
	if dec, ok := decisionFromContext(ctx); ok {
		// After the rewrite addr is the validated IP:port, not the
		// original hostname the trusted origin names. Trust then comes
		// from the decision made for exactly this target address.
		if decHost, decPort, err := net.SplitHostPort(dec.targetAddr); err == nil &&
			hostPortKey(decHost, decPort) == hostPortKey(host, port) {
			trusted = dec.trusted
		}
	}
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

// policyTransport builds one policy pool (trusted or public) from
// base. Only a *http.Transport base can be guarded: it is cloned —
// Clone gives each clone its own connection pool — into two variants:
//
//   - direct: Proxy forced to nil and the dial hooks replaced by the
//     pool's guarded dialer, so the destination is dialed at the
//     validated (pinned) IP in the route decision. Dial/DialTLS,
//     when set, would bypass DialContext, so they are dropped;
//     DialTLSContext is replaced by the guard's own, which dials the
//     IP and uses the original hostname as TLS ServerName (from the
//     request's route decision in context).
//   - proxied: the base's dial hooks (to the PROXY, the owner's own
//     network origin, which may legitimately be private) are preserved,
//     but Proxy is replaced by proxyFromDecision: it only returns the
//     proxy selected ONCE by policyTransport.RoundTrip for this
//     request, stored in the request context. The owner's selector
//     (ht.Proxy) is kept separately in the selector field and is never
//     wired into a transport. DialTLS/DialTLSContext are dropped: they
//     would create TLS straight to the destination, outside the proxy
//     tunnel (TLS to the destination over CONNECT is done by the
//     transport; for HTTPS it runs on a per-request clone, see
//     RoundTrip). HTTP claro via proxy nem usa esta variante: é escrito
//     pela guarda (roundTripViaHTTPProxy), porque net/http montaria a
//     forma absoluta a partir de req.Host.
//
// Any other RoundTripper has no dialer or pool the guard can split or
// reconfigure, so it yields unsupportedTransport: requests fail with
// ErrUnsupportedTransport rather than fetching without enforcement.
func (g *Guard) policyTransport(base http.RoundTripper, trusted bool) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	ht, ok := base.(*http.Transport)
	if !ok {
		return unsupportedTransport{base: base}
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dec, ok := decisionFromContext(ctx)
		if !ok || dec.trusted != trusted || addr != dec.poolAddr {
			return nil, fmt.Errorf("%w: invalid direct route", ErrBlocked)
		}
		var lastErr error
		_, port, _ := net.SplitHostPort(dec.targetAddr)
		for _, ip := range dec.ips {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			conn, err := g.dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	direct := ht.Clone()
	direct.Proxy = nil
	// Bound the total idle connections even when the supplied transport has
	// no global limit; pool keys include the hostname and validated address set.
	if direct.MaxIdleConns <= 0 {
		direct.MaxIdleConns = 100
	}
	direct.DialContext = dial
	direct.Dial = nil
	direct.DialTLSContext = g.dialTLSContext(dial, ht.TLSClientConfig)
	direct.DialTLS = nil
	direct.OnProxyConnectResponse = nil
	proxied := ht.Clone()
	proxied.Proxy = proxyFromDecision
	// DialTLS/DialTLSContext criariam TLS direto no destino, fora do
	// túnel do proxy; descartados também aqui (o TLS com o destino via
	// CONNECT é feito pelo transporte sobre o dial do proxy).
	proxied.DialTLSContext = nil
	proxied.DialTLS = nil
	// Dial para o proxy em si (HTTP claro via proxy, escrito à mão).
	proxyDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	if ht.DialContext != nil {
		proxyDial = ht.DialContext
	} else if ht.Dial != nil {
		proxyDial = func(_ context.Context, network, addr string) (net.Conn, error) {
			return ht.Dial(network, addr)
		}
	}
	return policyTransport{
		guard:     g,
		trusted:   trusted,
		selector:  ht.Proxy,
		tlsConfig: ht.TLSClientConfig,
		proxyDial: proxyDial,
		direct:    direct,
		proxied:   proxied,
	}
}

// CloseIdleConnections releases the guarded pools when the client is retired.
func (t transport) CloseIdleConnections() {
	for _, rt := range []http.RoundTripper{t.public, t.trusted} {
		if c, ok := rt.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
}

func (p policyTransport) CloseIdleConnections() {
	p.direct.CloseIdleConnections()
	p.proxied.CloseIdleConnections()
}

// unsupportedTransport refuses every request: the guard cannot pin or
// validate dials made inside a custom RoundTripper, so it must not
// fetch through one at all.
type unsupportedTransport struct {
	base http.RoundTripper
}

func (u unsupportedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: %T, want *http.Transport", ErrUnsupportedTransport, u.base)
}

// policyTransport is one policy's pool pair. Per request (per
// RoundTrip, so per redirect hop) it decides the route ONCE, in this
// order:
//
//  1. Resolve e valida todos os endereços: público (qualquer
//     candidato não-público recusa), ou — só nesta pool confiável e só
//     para exatamente a origem do dono — endereços mesmo privados.
//     Destino recusado aqui nunca toca seletor, proxy nem destino.
//  2. Chama o seletor do dono (selector: a Proxy func do transporte
//     base, que em http.DefaultTransport é ProxyFromEnvironment) UMA
//     vez, com a requisição original. Proxy com scheme não-http ou sem
//     host não pode ser amarrado ao IP validado: ErrUnsupportedTransport.
//  3. Guarda a decisão no contexto de um clone da requisição, reescreve
//     o host do clone para o IP validado (IP:port literal) preservando
//     req.Host, e entrega o clone à variante da rota decidida. A Proxy
//     func da variante via-proxy (proxyFromDecision) só devolve o proxy
//     já decidido do contexto — o seletor não roda de novo — e, direto,
//     o DialContext/DialTLSContext recebem o IP literal, sem nova
//     resolução. Via proxy, CONNECT/forma absoluta levam o IP literal:
//     o proxy não resolve o hostname.
type policyTransport struct {
	guard   *Guard
	trusted bool
	// selector is the owner's Proxy function from the base transport.
	// It is called exactly once per RoundTrip (here), never by the
	// inner transports.
	selector  func(*http.Request) (*url.URL, error)
	tlsConfig *tls.Config // base template; cloned, never mutated
	proxyDial func(ctx context.Context, network, addr string) (net.Conn, error)
	direct    *http.Transport
	proxied   *http.Transport
}

// supportedProxy accepts the schemes that can be pinned to a validated IP.
func supportedProxy(u *url.URL) error {
	if u == nil {
		return nil
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		if u.Hostname() == "" {
			return fmt.Errorf("%w: proxy has no host", ErrUnsupportedTransport)
		}
		return nil
	default:
		return fmt.Errorf("%w: proxy scheme %q is not supported", ErrUnsupportedTransport, u.Scheme)
	}
}

func (p policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Hostname() == "" {
		return nil, fmt.Errorf("%w: %q is not an http(s) URL with a host", ErrBlocked, req.URL.Redacted())
	}
	if p.trusted && !isTrustedRequest(req) {
		return nil, fmt.Errorf("%w: %q is not the trusted origin", ErrBlocked, req.URL.Redacted())
	}
	ctx := req.Context()
	// 1) Resolver+validar o destino ANTES de seletor/proxy/dial.
	ips, err := p.guard.resolveValidatedIPs(ctx, req.URL.Hostname(), p.trusted)
	if err != nil {
		return nil, err
	}
	ip := ips[0]
	// 2) Selecionar a rota UMA vez, sobre a requisição original.
	// A trusted destination that is not public stays direct: the proxy
	// must not be asked to dial a LAN address the guard already accepted.
	var proxyURL *url.URL
	if IsPublicIP(ip) && p.selector != nil {
		u, err := p.selector(req)
		if err != nil {
			return nil, err
		}
		proxyURL = u
	}
	if err := supportedProxy(proxyURL); err != nil {
		return nil, err
	}
	// 3) Decisão no contexto + rewrite do host para o IP validado.
	port := req.URL.Port()
	if port == "" {
		port = "80"
		if req.URL.Scheme == "https" {
			port = "443"
		}
	}
	dec := &routeDecision{
		trusted:    p.trusted,
		proxy:      proxyURL,
		ip:         ip,
		ips:        ips,
		host:       req.URL.Hostname(),
		targetAddr: net.JoinHostPort(ip.String(), port),
	}
	bound := req.Clone(context.WithValue(ctx, routeDecisionKey{}, dec))
	// Include the entire validated set in the pool key: a connection to any
	// candidate can only be reused when that complete set is still approved.
	var poolKey strings.Builder
	for _, candidate := range ips {
		fmt.Fprintf(&poolKey, "%x-", candidate.As16())
	}
	dec.poolAddr = net.JoinHostPort(fmt.Sprintf("%s.%x", poolKey.String(), strings.ToLower(dec.host)), port)
	bound.URL.Host = dec.targetAddr
	if bound.Host == "" {
		// Preserve the original Host header: with the URL rewritten to
		// an IP, the transport would otherwise send the IP as Host.
		bound.Host = req.URL.Host
	}
	var resp *http.Response
	switch {
	case proxyURL == nil:
		bound.URL.Host = dec.poolAddr
		resp, err = p.direct.RoundTrip(bound)
	case proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" || bound.URL.Scheme == "https":
		// TLS ao destino sobre o CONNECT: o ServerName precisa ser o
		// hostname original, e o TLSClientConfig do transporte
		// compartilhado não pode ser mutado por requisição. Clone por
		// requisição (pool próprio só neste modo); a Proxy func do
		// clone continua sendo proxyFromDecision, que consome a
		// decisão do contexto sem re-selecionar. O CONNECT em si sai
		// do URL reescrito: IP literal, o proxy não resolve o nome.
		cloned := p.proxied.Clone()
		cfg := cloneTLSConfig(p.tlsConfig)
		if cfg.ServerName == "" {
			cfg.ServerName = dec.host
		}
		cloned.TLSClientConfig = cfg
		cloned.DisableKeepAlives = true
		resp, err = cloned.RoundTrip(bound)
	default:
		// HTTP claro via proxy, escrito pela guarda (abaixo).
		resp, err = p.roundTripViaHTTPProxy(bound, dec)
	}
	if resp != nil {
		resp.Request = req
	}
	return resp, err
}

// roundTripViaHTTPProxy sends one plain-HTTP request through the
// decided http proxy on a dedicated connection. It exists because
// net/http's WriteProxy builds the absolute-form URI from req.Host:
// delegating to http.Transport would either send the hostname to the
// proxy (which would resolve it — the v4 hole) or lose the original
// Host header. Writing the request here sends BOTH: request-line with
// the validated IP literal, Host header with the original host. The
// connection is not pooled (closed with the response body); that is
// the declared cost of this mode. Proxy auth comes from the proxy
// URL's userinfo, as http.Transport would do.
func (p policyTransport) roundTripViaHTTPProxy(bound *http.Request, dec *routeDecision) (*http.Response, error) {
	if !httpguts.ValidHostHeader(bound.Host) || (bound.Method != "" && !httpguts.ValidHeaderFieldName(bound.Method)) {
		if bound.Body != nil {
			bound.Body.Close()
		}
		return nil, errors.New("safefetch: invalid request host or method")
	}
	for k, values := range bound.Header {
		if !httpguts.ValidHeaderFieldName(k) {
			if bound.Body != nil {
				bound.Body.Close()
			}
			return nil, fmt.Errorf("safefetch: invalid header %q", k)
		}
		for _, v := range values {
			if !httpguts.ValidHeaderFieldValue(v) {
				if bound.Body != nil {
					bound.Body.Close()
				}
				return nil, fmt.Errorf("safefetch: invalid header value for %q", k)
			}
		}
	}
	if len(bound.Trailer) != 0 {
		if bound.Body != nil {
			bound.Body.Close()
		}
		return nil, fmt.Errorf("%w: proxy request trailers", ErrUnsupportedTransport)
	}
	proxy := dec.proxy
	proxyPort := proxy.Port()
	if proxyPort == "" {
		proxyPort = "80"
		if proxy.Scheme == "https" {
			proxyPort = "443"
		}
	}
	conn, err := p.proxyDial(bound.Context(), "tcp", net.JoinHostPort(proxy.Hostname(), proxyPort))
	if err != nil {
		if bound.Body != nil {
			bound.Body.Close()
		}
		return nil, err
	}
	if proxy.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(bound.Context()); err != nil {
			conn.Close()
			if bound.Body != nil {
				bound.Body.Close()
			}
			return nil, err
		}
		conn = tc
	}
	stop := context.AfterFunc(bound.Context(), func() { conn.Close() })

	bw := bufio.NewWriter(conn)
	fail := func(err error) (*http.Response, error) {
		stop()
		conn.Close()
		if bound.Body != nil {
			bound.Body.Close()
		}
		return nil, err
	}
	method := bound.Method
	if method == "" {
		method = http.MethodGet
	}
	// bound.URL.Host já é o IP validado (rewrite em RoundTrip): a forma
	// absoluta sai dele — sem o rewrite, sairia o hostname.
	if _, err := fmt.Fprintf(bw, "%s http://%s%s HTTP/1.1\r\n", method, bound.URL.Host, bound.URL.RequestURI()); err != nil {
		return fail(err)
	}
	if _, err := fmt.Fprintf(bw, "Host: %s\r\n", bound.Host); err != nil {
		return fail(err)
	}
	header := bound.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	for _, k := range []string{"Host", "Content-Length", "Transfer-Encoding", "Connection", "Proxy-Connection", "Proxy-Authorization", "Trailer"} {
		header.Del(k)
	}
	if _, hasUA := header["User-Agent"]; !hasUA {
		header.Set("User-Agent", "Go-http-client/1.1")
	}
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(proxy.User.Username() + ":" + password))
		header.Set("Proxy-Authorization", "Basic "+cred)
	} else if pa := bound.Header.Get("Proxy-Authorization"); pa != "" {
		header.Set("Proxy-Authorization", pa)
	}
	if err := header.Write(bw); err != nil {
		return fail(err)
	}
	hasBody := bound.Body != nil && bound.Body != http.NoBody
	chunked := hasBody && bound.ContentLength < 0
	switch {
	case chunked:
		if _, err := io.WriteString(bw, "Transfer-Encoding: chunked\r\n"); err != nil {
			return fail(err)
		}
	case hasBody || bound.ContentLength > 0:
		if _, err := fmt.Fprintf(bw, "Content-Length: %d\r\n", bound.ContentLength); err != nil {
			return fail(err)
		}
	}
	if _, err := io.WriteString(bw, "Connection: close\r\n\r\n"); err != nil {
		return fail(err)
	}
	if hasBody {
		if chunked {
			buf := make([]byte, 32<<10)
			for {
				n, rerr := bound.Body.Read(buf)
				if n > 0 {
					if _, err := fmt.Fprintf(bw, "%x\r\n", n); err != nil {
						return fail(err)
					}
					if _, err := bw.Write(buf[:n]); err != nil {
						return fail(err)
					}
					if _, err := io.WriteString(bw, "\r\n"); err != nil {
						return fail(err)
					}
					if err := bw.Flush(); err != nil {
						return fail(err)
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					return fail(rerr)
				}
			}
			if _, err := io.WriteString(bw, "0\r\n\r\n"); err != nil {
				return fail(err)
			}
		} else if _, err := io.CopyN(bw, bound.Body, bound.ContentLength); err != nil {
			return fail(err)
		}
		bound.Body.Close()
	}
	if err := bw.Flush(); err != nil {
		return fail(err)
	}
	if timeout := p.proxied.ResponseHeaderTimeout; timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	}
	br := bufio.NewReader(conn)
	var resp *http.Response
	for n := 0; ; n++ {
		resp, err = http.ReadResponse(br, bound)
		if err != nil {
			return fail(err)
		}
		if resp.StatusCode < 100 || resp.StatusCode >= 200 || resp.StatusCode == 101 {
			break
		}
		resp.Body.Close()
		if n >= 9 {
			return fail(errors.New("safefetch: too many informational responses"))
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	resp.Request = bound
	resp.Body = &proxyConnBody{ReadCloser: resp.Body, conn: conn, stop: stop}
	return resp, nil
}

// proxyConnBody ties the dedicated proxy connection's lifetime to the
// response body: closing the body closes the connection (no pooling
// for plain-HTTP-via-proxy, see roundTripViaHTTPProxy).
type proxyConnBody struct {
	io.ReadCloser
	conn net.Conn
	stop func() bool
}

func (b *proxyConnBody) Close() error {
	err := b.ReadCloser.Close()
	b.stop()
	return errors.Join(err, b.conn.Close())
}

// Transport returns a RoundTripper that resolves and validates each
// destination once per request and binds connections to the validated IP,
// with
// separate trusted-origin and public connection pools (see the package
// documentation). A nil base means http.DefaultTransport. A base that
// is not a *http.Transport yields a RoundTripper whose requests all
// fail with ErrUnsupportedTransport, as does a proxy scheme other than
// http, https, or socks5.
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
