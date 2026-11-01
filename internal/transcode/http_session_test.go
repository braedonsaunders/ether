package transcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func httpInfo() core.MediaInfo {
	return core.MediaInfo{
		Duration: time.Minute,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
		},
	}
}

func TestHTTPSessionServesMatroska(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	hs, err := m.StartHLS(context.Background(), core.HLSRequest{
		SessionID:   "http-1",
		InputURL:    "http://127.0.0.1/src",
		Info:        httpInfo(),
		Decision:    core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mkv"},
		Protocol:    core.ProtocolHTTP,
		StartOffset: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, ok := hs.(core.HTTPSession)
	if !ok {
		t.Fatalf("StartHLS returned %T, want HTTPSession", hs)
	}
	if got, found := m.Session("http-1"); !found || got.ID() != "http-1" {
		t.Fatal("Session lookup missed HTTP session")
	}

	rec := httptest.NewRecorder()
	stream.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("ct %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Accept-Ranges") != "none" {
		t.Fatalf("accept-ranges %q", rec.Header().Get("Accept-Ranges"))
	}
	if !strings.Contains(rec.Body.String(), "fake-matroska") {
		t.Fatalf("body %q", rec.Body.String())
	}

	args := fr.lastStart()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-f matroska") || !strings.Contains(joined, "pipe:1") {
		t.Fatalf("args %q missing matroska pipe", joined)
	}
	if !strings.Contains(joined, "-ss") || !strings.Contains(joined, "-copyts") {
		t.Fatalf("args %q missing VOD seek", joined)
	}
	if strings.Contains(joined, "-c:a copy") || strings.Contains(joined, "-c copy") {
		t.Fatalf("args %q copied AAC into piped Matroska", joined)
	}

	head := httptest.NewRecorder()
	stream.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("head: %d len=%d", head.Code, head.Body.Len())
	}

	m.Stop("http-1")
	if _, found := m.Session("http-1"); found {
		t.Fatal("Stop left HTTP session")
	}
	closed := httptest.NewRecorder()
	stream.ServeHTTP(closed, httptest.NewRequest(http.MethodGet, "/", nil))
	if closed.Code != http.StatusGone {
		t.Fatalf("closed session: %d", closed.Code)
	}
}
