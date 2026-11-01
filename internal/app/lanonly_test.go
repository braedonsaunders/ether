package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLANOnly(t *testing.T) {
	h := lanOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for addr, want := range map[string]int{
		"127.0.0.1:5000":  http.StatusOK,
		"192.168.1.20:1":  http.StatusOK,
		"10.0.0.43:1":     http.StatusOK,
		"100.101.102.1:1": http.StatusOK, // Tailscale
		"[fd00::1]:1":     http.StatusOK,
		"203.0.113.9:1":   http.StatusForbidden,
		"[2001:db8::1]:1": http.StatusForbidden,
		"not-an-address":  http.StatusForbidden,
	} {
		r := httptest.NewRequest("GET", "/ether/", nil)
		r.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", addr, rec.Code, want)
		}
	}
}

func TestLANOnlyForwardedPublicClient(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"} {
		for _, peer := range []string{"127.0.0.1:1234", "10.0.0.2:1234"} {
			h := lanOnly(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), func() []string { return []string{"10.0.0.2/32"} })
			r := httptest.NewRequest("GET", "/ether/", nil)
			r.RemoteAddr = peer
			value := "203.0.113.1"
			if header == "Forwarded" {
				value = "for=" + value
			}
			r.Header.Set(header, value)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s %s: %d", peer, header, w.Code)
			}
		}
	}
}
