package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperationDriftFollowsReferences(t *testing.T) {
	a, err := operations([]byte(`{"paths":{"/Items/{id}":{"get":{"responses":{"200":{"schema":{"$ref":"#/definitions/Item"}}}}}},"definitions":{"Item":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := operations([]byte(`{"paths":{"/Items/{id}":{"get":{"responses":{"200":{"schema":{"$ref":"#/definitions/Item"}}}}},"/new":{"post":{}}},"definitions":{"Item":{"type":"integer"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	changes := diff("jellyfin", a, b, map[string]bool{"GET /Items/{}": true}, nil)
	if len(changes) != 2 {
		t.Fatalf("changes: %+v", changes)
	}
	found := false
	for _, c := range changes {
		if c.Path == "/Items/{id}" && c.Kind == "changed" && c.Priority == "used-by-clients" {
			found = true
		}
	}
	if !found {
		t.Fatalf("not tagged: %+v", changes)
	}
}
func TestFetchFailureAndChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer srv.Close()
	if _, err := fetch(srv.URL); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("failure: %v", err)
	}
	if err := verify([]byte("wrong"), strings.Repeat("0", 64)); err == nil {
		t.Fatal("incorrect checksum accepted")
	}
}
func TestTemplateShapeIgnoresQuery(t *testing.T) {
	if got := key("GET", "/Items/${item_id}?Fields=ProviderIds"); got != "GET /Items/{}" {
		t.Fatalf("shape: %q", got)
	}
}
func TestLatestJellyfinRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Write([]byte(`<a href="jellyfin-openapi-9.20.json">old</a><a href="jellyfin-openapi-12.4.json">new</a><a href="jellyfin-openapi-12.4-rc1.json">rc</a>`))
			return
		}
		w.Write([]byte(r.URL.Path))
	}))
	defer srv.Close()
	b, err := latestSpec(spec{Protocol: "jellyfin", LatestURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "/jellyfin-openapi-12.4.json" {
		t.Fatalf("selected %s", b)
	}
}
