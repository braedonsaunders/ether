//go:build conformlab && plexparity && !scorecard

package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConformLabSurfaceUsesOnlyVerifiedFixtureListener(t *testing.T) {
	r := httptest.NewRequest("GET", "http://attacker.invalid/", nil)
	r.RemoteAddr = "198.51.100.5:1234"
	r.Header.Set("X-Forwarded-Host", "attacker.invalid")
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18196}))
	got, err := conformLabLoopbackURL(r)
	if err != nil || got != "http://127.0.0.1:18196" {
		t.Fatal(got, err)
	}
	r.RemoteAddr = "203.0.113.5:1234"
	if _, err = conformLabLoopbackURL(r); err == nil {
		t.Fatal("non-fixture peer accepted")
	}
	r.RemoteAddr = "127.0.0.1:1234"
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 18196}))
	if _, err = conformLabLoopbackURL(r); err == nil {
		t.Fatal("non-loopback listener accepted")
	}
}
