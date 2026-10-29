//go:build scorecard && !plexparity

package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScorecardSurfaceUsesOnlyVerifiedLoopback(t *testing.T) {
	req := httptest.NewRequest("GET", "http://attacker.example/path", nil)
	req.RemoteAddr = "127.0.0.1:1000"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18096}))
	got, err := scorecardLoopbackURL(req)
	if err != nil || got != "http://127.0.0.1:18096" {
		t.Fatalf("origin %q %v", got, err)
	}
	req.RemoteAddr = "203.0.113.9:1000"
	if _, err = scorecardLoopbackURL(req); err == nil {
		t.Fatal("non-loopback peer accepted")
	}
	req.RemoteAddr = "127.0.0.1:1000"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 18096}))
	if _, err = scorecardLoopbackURL(req); err == nil {
		t.Fatal("unverified listener accepted")
	}
}
