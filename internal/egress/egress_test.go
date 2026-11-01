package egress

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestRouteMatrix(t *testing.T) {
	var hits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	u, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	prev := Current()
	t.Cleanup(func() { Set(prev) })

	Set(Policy{Enabled: true, Proxy: u, Indexers: true, Stremio: true, Debrid: true, Usenet: true})
	if err := get(CapIndexers, "http://indexer.example/search"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("indexer hits %d", hits)
	}
	before := hits
	_ = get(CapDirect, "http://indexer.example/plex")
	_ = get(CapMetadata, "http://indexer.example/meta")
	if hits != before {
		t.Fatalf("direct or metadata used the proxy: %d -> %d", before, hits)
	}

	Set(Policy{Enabled: true, Proxy: u, Metadata: true})
	if err := get(CapMetadata, "http://indexer.example/meta"); err != nil {
		t.Fatal(err)
	}
	if hits != before+1 {
		t.Fatalf("metadata opt-in hits %d", hits)
	}

	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer local.Close()
	before = hits
	if err := get(CapIndexers, local.URL); err != nil {
		t.Fatal(err)
	}
	if hits != before {
		t.Fatal("loopback was proxied")
	}
}

func TestFailClosedDoesNotDialDirect(t *testing.T) {
	prev := Current()
	t.Cleanup(func() { Set(prev) })
	Set(Policy{Enabled: true, Indexers: true})
	err := get(CapIndexers, "http://indexer.example/search")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err %v, want not configured", err)
	}
}

func get(cap Capability, raw string) error {
	c := Client(cap, time.Second)
	resp, err := c.Get(raw)
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return err
}
