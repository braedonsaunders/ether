package config

import (
	"strings"
	"testing"
)

func TestOutboundProblems(t *testing.T) {
	ok := Outbound{Enabled: true, Preset: "gluetun", URL: "http://gluetun:8888", Username: "ether", Password: "secret word", Indexers: true}
	if p := ok.Problems(); len(p) != 0 {
		t.Fatalf("valid proxy: %+v", p)
	}
	cases := []struct {
		name  string
		o     Outbound
		field string
	}{
		{"missing", Outbound{Enabled: true}, "outbound.url"},
		{"no port", Outbound{Enabled: true, URL: "http://gluetun"}, "outbound.url"},
		{"userinfo", Outbound{Enabled: true, URL: "http://user:pass@gluetun:8888"}, "outbound.url"},
		{"query", Outbound{Enabled: true, URL: "socks5://gluetun:1080?x=1"}, "outbound.url"},
		{"path", Outbound{Enabled: true, URL: "http://gluetun:8888/proxy"}, "outbound.url"},
		{"scheme", Outbound{Enabled: true, URL: "ftp://gluetun:21"}, "outbound.url"},
		{"preset", Outbound{Enabled: true, Preset: "wireguard", URL: "http://gluetun:8888"}, "outbound.preset"},
		{"user space", Outbound{Enabled: true, URL: "http://gluetun:8888", Username: "bad name"}, "outbound.username"},
		{"bypass", Outbound{URL: "http://gluetun:8888", Bypass: []string{"not a host"}}, "outbound.bypass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probs := tc.o.Problems()
			if len(probs) == 0 || probs[0].Field != tc.field {
				t.Fatalf("problems %+v, want field %s", probs, tc.field)
			}
			for _, p := range probs {
				if strings.Contains(p.Message, "gluetun") || strings.Contains(p.Message, "secret") || strings.Contains(p.Message, "user:pass") {
					t.Fatalf("message leaked input: %q", p.Message)
				}
			}
		})
	}
	off := Outbound{}
	if p := off.Problems(); len(p) != 0 {
		t.Fatalf("disabled blank proxy: %+v", p)
	}
}

func TestOutboundProxyURLStripsUserinfo(t *testing.T) {
	u, err := (Outbound{URL: "https://proxy.example:8443/"}).ProxyURL()
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "https://proxy.example:8443" {
		t.Fatalf("url %s", u)
	}
}
