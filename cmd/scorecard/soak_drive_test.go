package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunSoakInjectsFaults(t *testing.T) {
	var mu sync.Mutex
	var faults []string
	var ffmpeg atomic.Int32
	ftyp := append([]byte{0, 0, 0, 0x18}, []byte("ftyp")...)
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/__scorecard/telemetry":
			_ = json.NewEncoder(w).Encode(SoakSample{At: time.Now(), RSSBytes: 10 << 20, FDs: 4, Goroutines: 5, FFmpeg: int(ffmpeg.Load()), GPU: "no_device", Valid: true})
		case r.URL.Path == "/__scorecard/fault":
			var body struct{ Action string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			faults = append(faults, body.Action)
			mu.Unlock()
			signaled := 0
			if strings.Contains(body.Action, "encoder") {
				signaled = 1
				ffmpeg.Store(0)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "signaled": signaled})
		case r.URL.Path == "/library/sections":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/all"):
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"rk"}]}}`))
		case strings.Contains(r.URL.Path, "/library/metadata/"):
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"key":"/library/parts/1/file.mov"}]}]}]}}`))
		case strings.Contains(r.URL.Path, "/library/parts/"):
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(ftyp)
		case strings.Contains(r.URL.Path, "start.m3u8"):
			ffmpeg.Store(1)
			_, _ = w.Write([]byte("#EXTM3U\nseg.ts\n"))
		case strings.HasSuffix(r.URL.Path, "seg.ts"):
			_, _ = w.Write([]byte{0x47, 0x00})
		case r.URL.Path == "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"tok","User":{"Id":"user"}}`))
		case strings.Contains(r.URL.Path, "/Items") && r.Method == http.MethodGet && !strings.Contains(r.URL.Path, "PlaybackInfo"):
			if strings.Contains(r.URL.RawQuery, "IncludeItemTypes") || strings.Contains(r.URL.Path, "/Users/") {
				_, _ = w.Write([]byte(`{"Items":[{"Id":"movie"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"MediaSources":[{"Id":"source"}]}`))
		case strings.Contains(r.URL.Path, "PlaybackInfo"):
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if strings.Contains(string(body), `"DirectPlayProfiles":[]`) {
				_, _ = w.Write([]byte(`{"PlaySessionId":"s","MediaSources":[{"Id":"source","TranscodingUrl":"/video/master.m3u8"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"PlaySessionId":"s","MediaSources":[{"Id":"source"}]}`))
		case strings.Contains(r.URL.Path, "master.m3u8"):
			_, _ = w.Write([]byte("#EXTM3U\n/video/seg.ts\n"))
		case strings.Contains(r.URL.Path, "/Videos/"):
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(ftyp)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
	plex := httptest.NewServer(http.HandlerFunc(handler))
	defer plex.Close()
	jellyfin := httptest.NewServer(http.HandlerFunc(handler))
	defer jellyfin.Close()
	emby := httptest.NewServer(http.HandlerFunc(handler))
	defer emby.Close()
	manifest, err := RunSoak(context.Background(), SoakConfig{
		Profile: "shakedown", Plex: plex.URL, Jellyfin: jellyfin.URL, Emby: emby.URL,
		User: "viewer", Password: "secret", PlexToken: "token", GPU: "no_device", Workers: 2,
		Timing: soakTiming{Steady: 200 * time.Millisecond, Fault: 250 * time.Millisecond, Followup: 200 * time.Millisecond, Cleanup: 80 * time.Millisecond, Sample: 40 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, fault := range manifest.Faults {
		got[fault.Action] = fault.OK
	}
	for _, action := range []string{"source_outage", "slow_upstream", "encoder_crash", "encoder_kill", "db_stall"} {
		if !got[action] {
			t.Fatalf("missing %s in %+v", action, manifest.Faults)
		}
	}
	protocols := map[string]int{}
	for _, op := range manifest.Ops {
		if op.Success {
			protocols[op.Protocol]++
		}
	}
	if protocols["plex"] == 0 || protocols["jellyfin"] == 0 || protocols["emby"] == 0 {
		t.Fatalf("protocols %+v ops %d", protocols, len(manifest.Ops))
	}
}
