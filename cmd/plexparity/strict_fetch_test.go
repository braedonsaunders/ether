package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

func TestStrictFetchRejectsMissingDiscovery(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer s.Close()
	req := protocol.Request{Method: http.MethodGet, Path: "/library/sections/{{section}}/all"}
	for _, vars := range []map[string]string{nil, {"section": ""}, {"section": "{{other}}"}} {
		if _, err := strictFetch(context.Background(), s.Client(), s.URL, "", req, "web", vars, false); err == nil {
			t.Fatalf("missing section accepted: %v", vars)
		}
	}
	if requests != 0 {
		t.Fatalf("missing fixtures made %d network requests", requests)
	}
	if _, err := strictFetch(context.Background(), s.Client(), s.URL, "", req, "web", map[string]string{"section": "2"}, false); err != nil || requests != 1 {
		t.Fatalf("known section: requests=%d err=%v", requests, err)
	}
}
