package identity

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientNet(t *testing.T) {
	tests := []struct {
		peer, header, value, want string
		lan                       bool
		trusted                   []netip.Prefix
	}{
		{"203.0.113.10:1", "X-Forwarded-For", "192.168.1.2", "203.0.113.10", false, nil},
		{"127.0.0.1:1", "X-Forwarded-For", "192.168.1.2, 203.0.113.8", "203.0.113.8", false, nil},
		{"127.0.0.1:1", "X-Forwarded-For", "garbage", "invalid IP", false, nil},
		{"127.0.0.1:1", "Forwarded", `for="[2001:db8::1]:123"`, "2001:db8::1", false, nil},
		{"10.0.0.2:1", "Forwarded", "for=203.0.113.5", "203.0.113.5", false, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
		{"127.0.0.1:1", "X-Forwarded-For", "198.51.100.7, 203.0.113.5", "198.51.100.7", false, []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}},
		{"127.0.0.1:1", "X-Forwarded-For", "192.168.1.4, 203.0.113.5", "192.168.1.4", true, []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}},
		{"127.0.0.1:1", "Forwarded", "for=unknown", "invalid IP", false, nil},
		{"127.0.0.1:1", "X-Forwarded-For", "192.168.1.4", "192.168.1.4", true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.peer+tt.value, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.peer
			r.Header.Set(tt.header, tt.value)
			ip, lan := ClientNet(r, tt.trusted...)
			if ip.String() != tt.want || lan != tt.lan {
				t.Fatalf("got %s %v, want %s %v", ip, lan, tt.want, tt.lan)
			}
		})
	}
}
