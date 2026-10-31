package admin

import (
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestConsoleConnectURLsReplaceContainerAddress(t *testing.T) {
	r := httptestRequest("192.168.68.66:38034")
	got := consoleConnectURLs(r, "jellyfin", []string{
		"http://172.22.0.2:8096",
		"http://172.22.0.2:38034/jellyfin",
	}, config.Config{}, nil)
	if len(got) != 1 || got[0] != "http://192.168.68.66:38034/jellyfin" {
		t.Fatalf("connect URLs = %#v", got)
	}
}

func TestConsoleConnectURLsKeepHostNetworkListener(t *testing.T) {
	r := httptestRequest("192.168.1.5:32400")
	got := consoleConnectURLs(r, "jellyfin", []string{
		"http://192.168.1.5:8096",
		"http://192.168.1.5:32400/jellyfin",
	}, config.Config{}, nil)
	if len(got) != 2 || got[0] != "http://192.168.1.5:32400/jellyfin" || got[1] != "http://192.168.1.5:8096" {
		t.Fatalf("connect URLs = %#v", got)
	}
}

func TestConsoleConnectURLsUseAdvertisedHost(t *testing.T) {
	r := httptestRequest("127.0.0.1:32400")
	cfg := config.Config{Plex: config.Plex{CustomURLs: []string{"https://ether.example:443"}}}
	got := consoleConnectURLs(r, "emby", []string{"http://172.20.0.2:8096"}, cfg, nil)
	if len(got) != 2 || got[0] != "https://ether.example:443/emby" || got[1] != "http://127.0.0.1:32400/emby" {
		t.Fatalf("connect URLs = %#v", got)
	}
}

func TestConsoleConnectURLsIgnoreUntrustedForwardedHost(t *testing.T) {
	r := httptestRequest("192.168.1.20:32400")
	r.Header.Set("X-Forwarded-Host", "attacker.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	got := consoleConnectURLs(r, "silo", nil, config.Config{}, nil)
	if len(got) != 1 || got[0] != "http://192.168.1.20:32400/silo" {
		t.Fatalf("connect URLs = %#v", got)
	}
}

func TestConsoleConnectURLsPlexUsesTheBrowserHost(t *testing.T) {
	r := httptestRequest("192.168.68.66:32803")
	got := consoleConnectURLs(r, "plex", []string{"http://172.20.0.2:32400"}, config.Config{}, nil)
	if len(got) != 1 || got[0] != "http://192.168.68.66:32803" {
		t.Fatalf("connect URLs = %#v", got)
	}
}

func TestConsoleConnectURLsTrustForwardedHostFromLoopback(t *testing.T) {
	r := httptestRequest("127.0.0.1:32400")
	r.RemoteAddr = "127.0.0.1:9"
	r.Header.Set("X-Forwarded-Host", "192.168.68.66:32802")
	r.Header.Set("X-Forwarded-Proto", "http")
	got := consoleConnectURLs(r, "jellyfin", []string{"http://172.20.0.2:8096"}, config.Config{}, nil)
	if len(got) != 1 || got[0] != "http://192.168.68.66:32802/jellyfin" {
		t.Fatalf("connect URLs = %#v", got)
	}
}

func httptestRequest(host string) *http.Request {
	r, err := http.NewRequest(http.MethodGet, "/ether/api/frontends/jellyfin/status", nil)
	if err != nil {
		panic(err)
	}
	r.Host = host
	r.RemoteAddr = "192.168.1.20:9"
	return r
}
