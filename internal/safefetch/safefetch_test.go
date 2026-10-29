package safefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.5.0.9", "::1", // loopback
		"10.0.0.1", "172.16.0.1", "172.31.255.1", "192.168.1.1", "fd00::1", "fc00::1", // private
		"169.254.169.254", "169.254.0.1", "fe80::1", // link-local / cloud metadata
		"100.64.0.1", "100.127.255.1", // CGNAT (repo treats it as local)
		"224.0.0.1", "ff02::1", // multicast
		"0.0.0.0", "::", // unspecified
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", // v4-in-v6 forms of blocked v4
		"not-an-ip", "",
	}
	for _, s := range blocked {
		ip, _ := netip.ParseAddr(s)
		if IsPublicIP(ip) {
			t.Errorf("IsPublicIP(%q) = true, want false", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	for _, s := range allowed {
		ip, err := netip.ParseAddr(s)
		if err != nil || !IsPublicIP(ip) {
			t.Errorf("IsPublicIP(%q) = false (err %v), want true", s, err)
		}
	}
}

func TestValidatePublicURL(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://127.0.0.1:8080/admin",
		"https://192.168.0.1/x",
		"http://[::1]/x",
		"ftp://8.8.8.8/x",
		"file:///etc/passwd",
		"http:///no-host",
		"8.8.8.8/no-scheme",
	} {
		if err := ValidatePublicURL(ctx, raw); !errors.Is(err, ErrBlocked) {
			t.Errorf("ValidatePublicURL(%q) = %v, want ErrBlocked", raw, err)
		}
	}
	if err := ValidatePublicURL(ctx, "https://8.8.8.8/x"); err != nil {
		t.Errorf("ValidatePublicURL(public literal) = %v", err)
	}
	// A host name that resolves privately (rebinding shape) is refused.
	g := &Guard{LookupIP: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
	}}
	if err := g.ValidatePublicURL(ctx, "http://rebind.test/x"); !errors.Is(err, ErrBlocked) {
		t.Errorf("ValidatePublicURL(rebinding host) = %v, want ErrBlocked", err)
	}
}

// fakeNetGuard maps public.test to a public IP and internal.test to
// loopback, and dials validated public IPs at the local test server.
// internal.test must never be dialed at all.
func fakeNetGuard(t *testing.T, serverAddr string, dialed *atomic.Int32) *Guard {
	t.Helper()
	return &Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "public.test":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			case "internal.test":
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("unknown host %q", host)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed.Add(1)
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if host != "93.184.216.34" {
				return nil, fmt.Errorf("guard dialed unvalidated address %q", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, serverAddr)
		},
	}
}

func TestGuardedClientPublicFakeAndInternalBlocked(t *testing.T) {
	var secretHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "public-ok")
		case "/redir":
			http.Redirect(w, r, "http://internal.test/secret", http.StatusFound)
		case "/secret":
			secretHits.Add(1)
			fmt.Fprint(w, "metadata-secret")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	var dialed atomic.Int32
	guard := fakeNetGuard(t, srv.Listener.Addr().String(), &dialed)
	client := guard.Client(&http.Client{Transport: &http.Transport{Proxy: nil}})

	// Public (fake) host is reachable; the body comes from the test server.
	resp, err := client.Get("http://public.test:" + port + "/ok")
	if err != nil {
		t.Fatalf("public fetch: %v", err)
	}
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if string(body[:n]) != "public-ok" {
		t.Fatalf("public fetch body = %q", body[:n])
	}

	// Direct private destination: refused before any dial or hit.
	if _, err := client.Get("http://internal.test:" + port + "/secret"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("internal fetch err = %v, want ErrBlocked", err)
	}
	if _, err := client.Get("http://169.254.169.254/latest/meta-data"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("metadata fetch err = %v, want ErrBlocked", err)
	}

	// Redirect from a public host to an internal one: refused on the hop.
	if _, err := client.Get("http://public.test:" + port + "/redir"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect fetch err = %v, want ErrBlocked", err)
	}
	if got := secretHits.Load(); got != 0 {
		t.Fatalf("internal handler hit %d times, want 0", got)
	}

	// Non-http(s) schemes are refused.
	if _, err := client.Get("file:///etc/passwd"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("file fetch err = %v, want ErrBlocked", err)
	}
}

// The two pools are distinct transports and neither uses a proxy,
// however the base transport was configured.
func TestGuardedTransportsHaveNoProxyAndSeparatePools(t *testing.T) {
	proxyURL, _ := url.Parse("http://proxy.test:8080")
	rt := (&Guard{}).Transport(&http.Transport{Proxy: http.ProxyURL(proxyURL)})
	tr, ok := rt.(transport)
	if !ok {
		t.Fatalf("Transport type = %T, want safefetch transport", rt)
	}
	pub, okPub := tr.public.(*http.Transport)
	trust, okTrust := tr.trusted.(*http.Transport)
	if !okPub || !okTrust {
		t.Fatalf("pool types = %T/%T, want two *http.Transport", tr.public, tr.trusted)
	}
	if pub == trust {
		t.Fatal("trusted and public pools are the same transport (shared connection pool)")
	}
	for name, pool := range map[string]*http.Transport{"public": pub, "trusted": trust} {
		if pool.Proxy != nil {
			t.Errorf("%s pool Proxy != nil; guarded fetches must never proxy", name)
		}
		if pool.DialContext == nil || pool.DialTLSContext != nil || pool.Dial != nil || pool.DialTLS != nil {
			t.Errorf("%s pool dial hooks not fully guarded (DialContext=%v DialTLSContext=%v Dial=%v DialTLS=%v)",
				name, pool.DialContext != nil, pool.DialTLSContext != nil, pool.Dial != nil, pool.DialTLS != nil)
		}
	}
}

// Regression (owner review, bypass 1): with a proxy configured on the
// base transport, an untrusted request to the cloud metadata address
// must reach neither the proxy nor the destination. The guard dials
// directly (Proxy=nil), so the metadata address is blocked at dial
// time. A public destination likewise goes direct, not via the proxy.
func TestGuardedClientNeverUsesConfiguredProxy(t *testing.T) {
	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		fmt.Fprint(w, "proxied")
	}))
	defer proxySrv.Close()
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "public-ok")
	}))
	defer targetSrv.Close()
	_, proxyPort, _ := net.SplitHostPort(strings.TrimPrefix(proxySrv.URL, "http://"))
	_, targetPort, _ := net.SplitHostPort(strings.TrimPrefix(targetSrv.URL, "http://"))

	// public.test and proxy.test both "resolve" to a public IP; the fake
	// dial connects to whichever real test server the validated port
	// names, so a proxied request observably lands on the proxy.
	guard := &Guard{
		LookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "public.test", "proxy.test":
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			return nil, fmt.Errorf("unknown host %q", host)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			real := proxySrv.Listener.Addr().String()
			if port == targetPort {
				real = targetSrv.Listener.Addr().String()
			}
			var d net.Dialer
			return d.DialContext(ctx, network, real)
		},
	}
	proxyURL, _ := url.Parse("http://proxy.test:" + proxyPort)
	client := guard.Client(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}})

	if _, err := client.Get("http://169.254.169.254/latest/meta-data"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("metadata fetch via configured proxy err = %v, want ErrBlocked", err)
	}
	resp, err := client.Get("http://public.test:" + targetPort + "/ok")
	if err != nil {
		t.Fatalf("public fetch with configured proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "public-ok" {
		t.Fatalf("public fetch body = %q, want direct %q", body, "public-ok")
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("proxy hit %d times, want 0 (guarded fetches ignore proxies)", got)
	}
}

// Regression (owner review, bypass 2): a connection opened to a
// private trusted origin lives in the trusted pool only. A later
// untrusted request to the very same host:port must be blocked — with
// a shared pool it would reuse the open connection and skip the dial
// (and its address check) entirely.
func TestGuardTrustedConnectionNotReusedByUntrustedRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "lan-ok")
	}))
	defer srv.Close()

	client := (&Guard{}).Client(&http.Client{})

	// Trusted fetch opens (and pools, in the trusted pool) a connection.
	ctx := WithTrustedOrigin(context.Background(), srv.URL)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/x", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("trusted LAN fetch: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// Untrusted request to the same host:port: blocked, no new hit.
	if _, err := client.Get(srv.URL + "/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("untrusted fetch to trusted host err = %v, want ErrBlocked", err)
	}
	// A context trusting a *different* origin does not unlock it either.
	otherCtx := WithTrustedOriginFor(context.Background(), "http://127.0.0.1:1", srv.URL+"/x")
	req, _ = http.NewRequestWithContext(otherCtx, http.MethodGet, srv.URL+"/x", nil)
	if _, err := client.Do(req); !errors.Is(err, ErrBlocked) {
		t.Fatalf("fetch trusting another origin err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 (only the trusted fetch)", got)
	}

	// The trusted pool still works afterwards.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/x", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("second trusted LAN fetch: %v", err)
	}
	resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Fatalf("hits = %d, want 2 (trusted fetches only)", got)
	}
}

func TestGuardTrustedOriginAllowsOnlyThatOrigin(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "lan-ok")
	}))
	defer srv.Close()

	client := (&Guard{}).Client(&http.Client{})
	ctx := WithTrustedOrigin(context.Background(), srv.URL)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/x", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("trusted LAN fetch: %v", err)
	}
	resp.Body.Close()

	// The same private host on a different port is not the trusted origin.
	other := strings.Replace(srv.URL, srv.Listener.Addr().String(), "127.0.0.1:1", 1)
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, other, nil)
	if _, err := client.Do(req); !errors.Is(err, ErrBlocked) {
		t.Fatalf("other private origin err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1 (only the trusted fetch)", got)
	}
}
