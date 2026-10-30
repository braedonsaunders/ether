package stremio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestImageBaseAllowsOnlyVerifiedLoopbackHTTP(t *testing.T) {
	for _, tc := range []struct {
		peer, base string
		allowed    bool
	}{
		{"127.0.0.1", "http://127.0.0.1:1234/stremio", true},
		{"::1", "http://[::1]:1234/stremio", true},
		{"192.168.1.2", "http://127.0.0.1:1234/stremio", false},
		{"127.0.0.1", "http://192.168.1.2:1234/stremio", false},
		{"127.0.0.1", "http://localhost:1234/stremio", false},
		{"127.0.0.1", "http://user:password@127.0.0.1/stremio", false},
		{"127.0.0.1", "http://127.0.0.1/stremio?token=secret", false},
		{"127.0.0.1", "http://127.0.0.1/stremio#secret", false},
	} {
		t.Run(tc.peer+tc.base, func(t *testing.T) {
			i := &instance{host: frontend.Host{
				ClientIP:   func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr(tc.peer), true },
				SurfaceURL: func(context.Context, *http.Request) (string, error) { return tc.base, nil },
			}}
			u := i.imageBase(httptest.NewRequest("GET", "http://untrusted.example/", nil))
			if (u != nil) != tc.allowed {
				t.Fatalf("origin allowed=%v want=%v", u != nil, tc.allowed)
			}
			if u != nil && u.String() != tc.base+"/image/" {
				t.Fatal(u)
			}
		})
	}
}
