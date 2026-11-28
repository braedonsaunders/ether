package egress

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
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

	Set(Policy{Enabled: true, Proxy: u, Indexers: true, Stremio: true, LiveTV: true, Debrid: true, Usenet: true})
	if err := get(CapIndexers, "http://indexer.example/search"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("indexer hits %d", hits)
	}
	if err := get(CapLiveTV, "http://provider.example/live"); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("Live TV hits %d", hits)
	}
	before := hits
	_ = get(CapDirect, "http://indexer.example/plex")
	_ = get(CapMetadata, "http://indexer.example/meta")
	if hits != before {
		t.Fatalf("direct or metadata used the proxy: %d -> %d", before, hits)
	}

	// Live TV alone through the proxy; add-ons stay direct.
	Set(Policy{Enabled: true, Proxy: u, LiveTV: true})
	if err := get(CapLiveTV, "http://provider.example/live"); err != nil {
		t.Fatal(err)
	}
	_ = get(CapStremio, "http://indexer.example/addon")
	if hits != before+1 {
		t.Fatalf("Live TV-only route hits %d, want %d", hits, before+1)
	}
	before = hits

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

func TestClientSharesTransportAndReusesConnections(t *testing.T) {
	prev := Current()
	t.Cleanup(func() { Set(prev) })
	Set(Policy{})

	c1 := Client(CapMetadata, 5*time.Second)
	c2 := Client(CapMetadata, 30*time.Second)
	if c1.Transport == nil || c1.Transport != c2.Transport {
		t.Fatal("Client calls for the same capability did not share a Transport")
	}
	if Client(CapDirect, 5*time.Second).Transport == c1.Transport {
		t.Fatal("distinct capabilities shared a Transport")
	}
	if Transport(CapMetadata) == c1.Transport {
		t.Fatal("Transport returned the shared Client transport instead of a customizable clone")
	}

	var dials atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dials.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	for i := 0; i < 3; i++ {
		if err := get(CapMetadata, srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("per-call Client opened %d connections, want 1", got)
	}

	// An already-created client's shared transport must follow policy changes.
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	pu, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	Set(Policy{Enabled: true, Proxy: pu, Metadata: true})
	resp, err := c1.Get("http://meta.example/item")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("proxy hits after Set = %d, want 1", got)
	}
}
