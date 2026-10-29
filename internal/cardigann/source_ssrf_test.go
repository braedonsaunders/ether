package cardigann

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// sourceWithDownloadURL serves a listing whose single entry points at
// downloadURL (remote, untrusted data).
func sourceWithDownloadURL(t *testing.T, downloadURL string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/listing" {
			json.NewEncoder(w).Encode([]map[string]string{
				{"name": "evil.yml", "type": "file", "sha": "sha-1", "download_url": downloadURL},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallFromSourceBlocksPrivateDownloadURL(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(readFixture(t, "public.yml"))
	}))
	defer internal.Close()

	src := sourceWithDownloadURL(t, internal.URL+"/evil.yml")
	s := newTestService(t, config.IndexerSites{})
	s.ApplyConfig(config.Config{IndexerSites: config.IndexerSites{DefinitionsURL: src.URL + "/listing"}})

	entries, err := s.Catalog(ctx, false)
	if err != nil || len(entries) != 1 {
		t.Fatalf("catalog = %+v err=%v", entries, err)
	}
	if _, err := s.InstallFromSource(ctx, "evil"); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("InstallFromSource(private download_url) err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("internal server hit %d times, want 0", got)
	}
}

// Regression (owner review): a proxy configured on the owner's HTTP
// client must not carry a listing-supplied metadata URL anywhere —
// guarded source fetches ignore proxies and validate the destination
// themselves.
func TestInstallFromSourceWithConfiguredProxyDoesNotReachProxy(t *testing.T) {
	ctx := context.Background()
	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.Write(readFixture(t, "public.yml"))
	}))
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)

	src := sourceWithDownloadURL(t, "http://169.254.169.254/latest/meta-data/evil.yml")
	s := newTestService(t, config.IndexerSites{})
	s.sourceHC = (&safefetch.Guard{}).Client(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}})
	s.ApplyConfig(config.Config{IndexerSites: config.IndexerSites{DefinitionsURL: src.URL + "/listing"}})
	if _, err := s.InstallFromSource(ctx, "evil"); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("InstallFromSource(metadata, proxy configured) err = %v, want ErrBlocked", err)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("proxy hit %d times, want 0", got)
	}
}

func TestInstallFromSourceBlocksMetadataDownloadURL(t *testing.T) {
	ctx := context.Background()
	src := sourceWithDownloadURL(t, "http://169.254.169.254/latest/meta-data/evil.yml")
	s := newTestService(t, config.IndexerSites{})
	s.ApplyConfig(config.Config{IndexerSites: config.IndexerSites{DefinitionsURL: src.URL + "/listing"}})
	if _, err := s.InstallFromSource(ctx, "evil"); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("InstallFromSource(metadata download_url) err = %v, want ErrBlocked", err)
	}
}

// A same-origin download that redirects off the source to a private
// host is refused on the redirect hop.
func TestInstallFromSourceRedirectToPrivateBlocked(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(readFixture(t, "public.yml"))
	}))
	defer internal.Close()

	var src *httptest.Server
	src = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/listing":
			json.NewEncoder(w).Encode([]map[string]string{
				{"name": "evil.yml", "type": "file", "sha": "sha-1", "download_url": src.URL + "/raw/evil.yml"},
			})
		case "/raw/evil.yml":
			http.Redirect(w, r, internal.URL+"/evil.yml", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer src.Close()

	s := newTestService(t, config.IndexerSites{})
	s.ApplyConfig(config.Config{IndexerSites: config.IndexerSites{DefinitionsURL: src.URL + "/listing"}})
	if _, err := s.InstallFromSource(ctx, "evil"); !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("InstallFromSource(redirect to private) err = %v, want ErrBlocked", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("internal server hit %d times, want 0", got)
	}
}
