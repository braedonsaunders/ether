package plex

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

func TestTracePlaybackLogsHLSRequestsOnly(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{
		log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		d:   Deps{TracePlayback: true},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("0123456789ab"))
	})
	handler := s.requestLog(next)
	req := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/session/abc/base/00039.mkv?X-Plex-Token=secret&offset=153", nil)
	req.Header.Set("Range", "bytes=0-")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "0123456789ab" {
		t.Fatalf("response %d %q", rec.Code, rec.Body.String())
	}
	line := logs.String()
	for _, want := range []string{
		"hls request",
		"method=GET",
		"path=/video/:/transcode/universal/session/abc/base/00039.mkv",
		"offset=153",
		"X-Plex-Token=redacted",
		`range="bytes=0-"`,
		"status=206",
		"bytes=12",
		"duration=",
	} {
		if !bytes.Contains(logs.Bytes(), []byte(want)) {
			t.Fatalf("log missing %q:\n%s", want, line)
		}
	}
	if bytes.Contains(logs.Bytes(), []byte("secret")) {
		t.Fatal(line)
	}
	logs.Reset()
	other := httptest.NewRecorder()
	handler.ServeHTTP(other, httptest.NewRequest(http.MethodGet, "/library/sections", nil))
	if bytes.Contains(logs.Bytes(), []byte("hls request")) {
		t.Fatal(logs.String())
	}
}

func TestUnauthorizedLogThrottlePreservesWire(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "unchanged")
		writeHTTPError(w, http.StatusUnauthorized)
	})
	handler := s.requestLog(next)
	control := httptest.NewRecorder()
	next.ServeHTTP(control, httptest.NewRequest("GET", "/", nil))
	var wg sync.WaitGroup
	for n := 0; n < 100; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("GET", "/secret?X-Plex-Token=secret", nil)
			got := httptest.NewRecorder()
			handler.ServeHTTP(got, r)
			if got.Code != control.Code || got.Body.String() != control.Body.String() || !reflect.DeepEqual(got.Header(), control.Header()) {
				t.Error("wire changed")
			}
		}()
	}
	wg.Wait()
	if bytes.Count(logs.Bytes(), []byte("authentication failed")) != 1 || bytes.Contains(logs.Bytes(), []byte("secret")) {
		t.Fatal(logs.String())
	}
}
