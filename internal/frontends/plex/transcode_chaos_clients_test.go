//go:build transcodechaos

package plex

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type chaosTranscoderRecorder struct {
	core.Transcoder
	requests []core.HLSRequest
}

func (r *chaosTranscoderRecorder) StartHLS(ctx context.Context, req core.HLSRequest) (core.HLSSession, error) {
	r.requests = append(r.requests, req)
	return r.Transcoder.StartHLS(ctx, req)
}

func TestTranscodeChaosClients(t *testing.T) {
	ff, fp := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	if ff == "" || fp == "" {
		t.Fatal("set CHAOS_FFMPEG and CHAOS_FFPROBE")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "source.mp4")
	chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "40", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac", "-movflags", "+faststart", file)
	source := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer source.Close()
	mk := func(t *testing.T, idle time.Duration) (*fixture, *transcode.Manager) {
		t.Helper()
		m, e := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(t.TempDir(), "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithSegmentTimeout(12*time.Second), transcode.WithIdleTimeout(idle), transcode.WithBuffering(4*time.Second, 16*time.Second, 16*time.Second))
		if e != nil {
			t.Fatal(e)
		}
		info, e := m.Probe(context.Background(), source.URL+"/source.mp4")
		if e != nil {
			t.Fatal(e)
		}
		info.Keyframes = nil
		f := newFixture()
		f.playback.info = *info
		f.srv.setPlayback(&chaosPlayback{f.playback, source.URL + "/source.mp4"})
		f.srv.setTranscoder(m)
		t.Cleanup(func() { f.srv.Close(); m.Close() })
		return f, m
	}
	start := func(id, extra string) string {
		return "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F" + fightClub.Key.String() + "&protocol=hls&directPlay=0&directStream=1&session=" + id + extra
	}
	get := func(t *testing.T, f *fixture, path, client string) []byte {
		t.Helper()
		r := f.get(t, path, "X-Plex-Client-Identifier", client)
		if r.Code != 200 {
			t.Fatalf("%s HTTP %d %s", path, r.Code, r.Body.String())
		}
		return r.Body.Bytes()
	}
	segment := func(id string, n int) string {
		return fmt.Sprintf("/video/:/transcode/universal/session/%s/base/%05d.ts", id, n)
	}
	decode := func(t *testing.T, b []byte) {
		t.Helper()
		p := filepath.Join(t.TempDir(), "segment.ts")
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		chaosCmd(t, fp, "-v", "error", "-show_streams", p)
		chaosCmd(t, ff, "-v", "error", "-xerror", "-i", p, "-f", "null", "-")
	}
	t.Run("rapid-unindexed-seeks", func(t *testing.T) {
		f, _ := mk(t, 2*time.Minute)
		get(t, f, start("seek", ""), "client-a")
		for _, n := range []int{0, 8, 1, 7, 2} {
			decode(t, get(t, f, segment("seek", n), "client-a"))
		}
		get(t, f, "/video/:/transcode/universal/stop?session=seek", "client-a")
	})
	t.Run("two-clients-abandon-restart", func(t *testing.T) {
		f, m := mk(t, 2*time.Minute)
		get(t, f, start("a", ""), "client-a")
		get(t, f, start("b", ""), "client-b")
		decode(t, get(t, f, segment("a", 0), "client-a"))
		decode(t, get(t, f, segment("b", 0), "client-b"))
		get(t, f, "/video/:/transcode/universal/ping?session=b", "client-b")
		get(t, f, "/video/:/transcode/universal/stop?session=a", "client-a")
		if _, ok := m.Session("b"); !ok {
			t.Fatal("stopping first client ended second")
		}
		get(t, f, start("a", ""), "client-a")
		decode(t, get(t, f, segment("a", 0), "client-a"))
		get(t, f, "/video/:/transcode/universal/stop?session=a", "client-a")
		get(t, f, "/video/:/transcode/universal/stop?session=b", "client-b")
	})
	t.Run("quality-change", func(t *testing.T) {
		f, m := mk(t, 2*time.Minute)
		get(t, f, start("quality", "&maxVideoBitrate=12000"), "client-a")
		decode(t, get(t, f, segment("quality", 0), "client-a"))
		first, ok := m.Session("quality")
		if !ok {
			t.Fatal("first session missing")
		}
		get(t, f, start("quality", "&maxVideoBitrate=100&videoResolution=160x90"), "client-a")
		second, ok := m.Session("quality")
		if !ok || first == second {
			t.Fatal("quality change did not replace session")
		}
		decode(t, get(t, f, segment("quality", 0), "client-a"))
		get(t, f, "/video/:/transcode/universal/stop?session=quality", "client-a")
	})
	t.Run("subtitle-burn-toggle", func(t *testing.T) {
		f, m := mk(t, 2*time.Minute)
		sub := filepath.Join(dir, "subtitle.srt")
		if err := os.WriteFile(sub, []byte("1\n00:00:00,500 --> 00:00:04,000\nSample subtitle\n"), 0600); err != nil {
			t.Fatal(err)
		}
		mkv := filepath.Join(dir, "subtitle.mkv")
		chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-i", file, "-i", sub, "-map", "0:v", "-map", "0:a", "-map", "1:s", "-c:v", "copy", "-c:a", "copy", "-c:s", "srt", mkv)
		info, err := m.Probe(context.Background(), source.URL+"/subtitle.mkv")
		if err != nil {
			t.Fatal(err)
		}
		f.playback.info = *info
		f.srv.setPlayback(&chaosPlayback{f.playback, source.URL + "/subtitle.mkv"})
		var subtitle core.StreamInfo
		for _, st := range info.Streams {
			if st.Type == "subtitle" {
				subtitle = st
				break
			}
		}
		if subtitle.Type == "" {
			t.Fatal("subtitle absent")
		}
		track := f.bindStream(fightClub.Key, subtitle.Index, subtitle)
		recorder := &chaosTranscoderRecorder{Transcoder: m}
		f.srv.setTranscoder(recorder)
		getWeb := func(path string) []byte {
			r := f.get(t, path, "X-Plex-Client-Identifier", "client-a", "X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome")
			if r.Code != 200 {
				t.Fatalf("%s HTTP %d", path, r.Code)
			}
			return r.Body.Bytes()
		}
		getWeb(start("subs", "&subtitleStreamID=0"))
		decode(t, getWeb(segment("subs", 0)))
		getWeb(start("subs", fmt.Sprintf("&subtitles=burn&subtitleStreamID=%d", track)))
		if len(recorder.requests) < 2 || !recorder.requests[len(recorder.requests)-1].Decision.BurnSubtitles {
			t.Fatal("subtitle change did not request burn")
		}
		decode(t, getWeb(segment("subs", 0)))
		getWeb("/video/:/transcode/universal/stop?session=subs")
	})
	t.Run("idle-timeout", func(t *testing.T) {
		f, m := mk(t, 500*time.Millisecond)
		get(t, f, start("idle", ""), "client-a")
		deadline := time.Now().Add(4 * time.Second)
		for {
			if _, ok := m.Session("idle"); !ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("idle session survived timeout")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}
