package main

import (
	"context"
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
