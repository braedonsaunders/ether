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

func TestBinaryPayloadIsNeverNormalized(t *testing.T) {
	for _, path := range []string{"image:/thumb", "playback:segment:transcode"} {
		a := capture{path: path, response: protocol.Response{Body: []byte("X-Ether-Stream-Token=one")}}
		b := capture{path: path, response: protocol.Response{Body: []byte("X-Ether-Stream-Token=two")}}
		if bytes.Equal(normalizeCaptureBody(a), normalizeCaptureBody(b)) {
			t.Fatalf("binary bytes normalized for %s", path)
		}
	}
}

func TestDelegationTokenNormalizationIsScoped(t *testing.T) {
	for _, bodies := range [][2]string{{`<MediaContainer token="one"/>`, `<MediaContainer token="two"/>`}, {`{"token":"one"}`, `{"token":"two"}`}} {
		a := capture{path: "/security/token", response: protocol.Response{Body: []byte(bodies[0])}}
		b := capture{path: "/security/token", response: protocol.Response{Body: []byte(bodies[1])}}
		if !bytes.Equal(normalizeCaptureBody(a), normalizeCaptureBody(b)) {
			t.Fatal("delegation token was not normalized")
		}
		a.path, b.path = "/identity", "/identity"
		if bytes.Equal(normalizeCaptureBody(a), normalizeCaptureBody(b)) {
			t.Fatal("token normalization escaped its route")
		}
	}
}

func TestWebsocketHandshakeTokenNormalizationIsScoped(t *testing.T) {
	c := capture{path: "/:/websockets/notifications", response: protocol.Response{Headers: http.Header{"Sec-Websocket-Accept": {"random-challenge"}, "Upgrade": {"websocket"}}}}
	got := normalizeCaptureHeaders(c)
	if got.Get("Sec-WebSocket-Accept") != "<WEBSOCKET-HANDSHAKE-TOKEN>" || got.Get("Upgrade") != "websocket" {
		t.Fatalf("headers=%v", got)
	}
	c.path = "/identity"
	if normalizeCaptureHeaders(c).Get("Sec-WebSocket-Accept") != "random-challenge" {
		t.Fatal("handshake normalization escaped websocket route")
	}
}
