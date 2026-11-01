package admin

import (
	"net/http"
	"os"
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

func TestValidateCustomURLsRejectsJunkAndKeepsCGNAT(t *testing.T) {
	base := config.Defaults(os.TempDir())
	base.Plex.CustomURLs = []string{
		"http://100.100.1.2:32400",
		"https://plex.example.com:8443",
	}
	if errs := ValidateConfig(base); len(errs) != 0 {
		t.Fatalf("expected valid custom URLs (including Tailscale CGNAT), got %+v", errs)
	}

	for _, junk := range []string{
		"[http://10.0.0.43:32450](http://10.0.0.43:32450/)",
		"plex.example.com:32400",
		"http://plex.example.com:32400/web",
	} {
		cfg := base
		cfg.Plex.CustomURLs = []string{junk}
		errs := ValidateConfig(cfg)
		var found *FieldError
		for i := range errs {
			if errs[i].Field == "plex.customUrls" {
				found = &errs[i]
				break
			}
		}
		if found == nil || found.Message != "Enter full http:// or https:// server URLs, separated by commas." {
			t.Fatalf("ValidateConfig(%q) = %+v, want plex.customUrls field error", junk, errs)
		}
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
