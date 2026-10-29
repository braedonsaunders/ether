package safefetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
