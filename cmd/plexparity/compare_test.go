package main

import (
	"bytes"
	"context"
	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompareRejectsMissingFixtures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<MediaContainer/>`)
	}))
	defer server.Close()
	_, err := compareCorpus(context.Background(), server.URL, server.URL, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "discovery incomplete") {
		t.Fatalf("missing fixture was not rejected: %v", err)
	}
}

func TestDiffTranscripts(t *testing.T) {
	a := transcript{ids: map[string]string{"movieKey": "7"}, captures: []capture{{viewer: "owner", client: "web", path: "/identity", response: protocol.Response{Status: 200, Body: []byte("old")}}}}
	b := transcript{ids: map[string]string{"movieKey": "7"}, captures: []capture{{viewer: "owner", client: "web", path: "/identity", response: protocol.Response{Status: 200, Body: []byte("new")}}}}
	var out bytes.Buffer
	n, err := diffTranscripts(a, b, &out)
	if err != nil || n != 1 || !strings.Contains(out.String(), "old") || !strings.Contains(out.String(), "new") {
		t.Fatalf("diff=%d output=%q error=%v", n, out.String(), err)
	}
	b.ids["movieKey"] = "8"
	if _, err := diffTranscripts(a, b, &out); err == nil {
		t.Fatal("mismatched fixture IDs accepted")
	}
}
