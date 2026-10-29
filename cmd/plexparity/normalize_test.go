package main

import (
	"bytes"
	"net/http"
	"testing"
)

func TestNormalizeOnlyStreamGrant(t *testing.T) {
	a := []byte("session/x/index.m3u8?X-Ether-Stream-Token=abc_123&quality=1")
	b := []byte("session/x/index.m3u8?X-Ether-Stream-Token=def-456&quality=1")
	c := []byte("session/x/index.m3u8?X-Ether-Stream-Token=def-456&quality=2")
	if !bytes.Equal(normalizeResponse(a), normalizeResponse(b)) {
		t.Fatal("grant was not normalized")
	}
	if bytes.Equal(normalizeResponse(a), normalizeResponse(c)) {
		t.Fatal("nonvolatile quality was hidden")
	}
}

func TestNormalizeHeadersOnlyDropsDate(t *testing.T) {
	h := http.Header{"Date": {"different"}, "Cache-Control": {"no-cache"}, "X-Test": {"value"}}
	got := normalizeHeaders(h)
	if got.Get("Date") != "" || got.Get("Cache-Control") != "no-cache" || got.Get("X-Test") != "value" || h.Get("Date") != "different" {
		t.Fatalf("unexpected normalized headers: %v", got)
	}
}
