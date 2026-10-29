package main

import (
	"bytes"
	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
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

func TestSegmentTimestampNormalizationIsScoped(t *testing.T) {
	h := http.Header{"Last-Modified": {"Thu, 29 Oct 2026 18:47:27 GMT"}}
	segment := capture{path: "playback:segment:remux", response: protocol.Response{Headers: h}}
	if normalizeCaptureHeaders(segment).Get("Last-Modified") != "" {
		t.Fatal("segment timestamp retained")
	}
	segment.path = "/library/metadata/1"
	if normalizeCaptureHeaders(segment).Get("Last-Modified") == "" {
		t.Fatal("nonsegment timestamp removed")
	}
}
