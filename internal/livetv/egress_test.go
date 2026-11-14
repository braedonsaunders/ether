package livetv

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/egress"
)

// Documents and the relay used by ffmpeg must share the live source policy.
func TestLiveTVEgress(t *testing.T) {
	previous := egress.Current()
	t.Cleanup(func() { egress.Set(previous) })
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "direct") }))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() || r.URL.Host != "provider.example" {
			t.Errorf("proxy target: %s", r.URL)
		}
		io.WriteString(w, "proxy")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	s := testService(t, Options{})
	transport := s.o.HTTPClient.Transport.(sourceUserAgentTransport).base.(*http.Transport)
	// Map the fixture's public hostname to our origin only for direct dials.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "provider.example:80" {
			address = origin.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	defer transport.CloseIdleConnections()
	for _, tc := range []struct {
		name   string
		policy egress.Policy
		want   string
	}{
		{"on", egress.Policy{Enabled: true, Proxy: proxyURL, Stremio: true}, "proxy"},
		{"off", egress.Policy{}, "direct"},
		{"source route off", egress.Policy{Enabled: true, Proxy: proxyURL}, "direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			egress.Set(tc.policy)
			for _, path := range []string{"playlist.m3u", "player_api.php", "guide.xml"} {
				body, err := s.openURL(t.Context(), "http://provider.example/"+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(body)
				body.Close()
				if err != nil || string(b) != tc.want {
					t.Fatalf("document %s = %q, %v", path, b, err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			g := &inputGrant{origin: "http://provider.example:80", headers: http.Header{}}
			res, err := s.relayFetch(ctx, cancel, g, "http://provider.example/live.ts", httptest.NewRequest(http.MethodGet, "/", nil))
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil || string(b) != tc.want {
				t.Fatalf("stream = %q, %v", b, err)
			}
		})
	}
}
