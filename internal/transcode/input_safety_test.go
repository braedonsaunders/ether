package transcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func assertInputSafety(t *testing.T, args []string, live, network bool) {
	t.Helper()
	formats := strings.Split(argValue(args, "-format_whitelist"), ",")
	protocols := strings.Split(argValue(args, "-protocol_whitelist"), ",")
	for _, name := range []string{"matroska", "mov", "avi", "mpegts", "asf", "mpeg", "ogg", "flv"} {
		if !slices.Contains(formats, name) {
			t.Errorf("supported VOD container %s missing: %v", name, args)
		}
	}
	for _, name := range []string{"hls", "dash", "webvtt"} {
		if slices.Contains(formats, name) != live {
			t.Errorf("format %s allowed=%v, live=%v", name, slices.Contains(formats, name), live)
		}
	}
	for _, name := range []string{"concat", "image2", "sdp"} {
		if slices.Contains(formats, name) {
			t.Errorf("unexpected external-resource demuxer %s", name)
		}
	}
	if slices.Contains(protocols, "file") == network || slices.Contains(protocols, "http") != network || slices.Contains(protocols, "pipe") {
		t.Errorf("unexpected input protocol permissions: %v", protocols)
	}
	for _, option := range []string{"-format_whitelist", "-protocol_whitelist"} {
		if i := slices.Index(args, option); i < 0 || i >= inputIndex(args) {
			t.Errorf("%s must apply before -i: %v", option, args)
		}
	}
}

func TestVODInputRestrictionsCoverEveryMediaCommand(t *testing.T) {
	input := "http://127.0.0.1:1/source/fixture"
	info := mustParse(t, "mkv_truehd_atmos.json")
	spec := baseSpec(info, Decide(info, appleTV, allow(), Policy{}))
	// Missing metadata must never silently grant live playlist privileges.
	spec.Info.Duration = 0
	for name, args := range map[string][]string{
		"probe":     probeArgs(input, probeAttempts[0]),
		"playback":  buildArgs(spec),
		"subtitle":  subtitleArgs(input, 0),
		"download":  downloadArgs(input, &core.StreamInfo{Index: 0}, nil, "output.mp4"),
		"thumbnail": thumbArgs(input, 0, 320, 3),
	} {
		t.Run(name, func(t *testing.T) { assertInputSafety(t, args, false, true) })
	}
	assertInputSafety(t, probeArgs("fixture.mkv", probeAttempts[0]), false, false)
}

func TestLiveInputRestrictionsRequireExplicitCallSiteChoice(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		fr := newFakeRunner()
		fr.probeJSON = []string{liveProbeJSON}
		m := newTestManager(t, fr, config.Transcode{})
		var err error
		if authenticated {
			_, err = m.ProbeLiveWithHeaders(context.Background(), "https://fixture.invalid/live", map[string]string{"Authorization": "Bearer fixture"})
		} else {
			_, err = m.ProbeLive(context.Background(), "https://fixture.invalid/live")
		}
		if err != nil {
			t.Fatal(err)
		}
		assertInputSafety(t, fr.probeCalls[0], true, true)
		req := remuxRequest(t, "live", time.Minute)
		req.LiveInput = true
		session := startSession(t, m, req)
		assertInputSafety(t, fr.lastStart(), true, true)
		if err := session.SwapInput(context.Background(), "https://fixture.invalid/replacement", req.Info, 0); err != nil {
			t.Fatal(err)
		}
		assertInputSafety(t, fr.lastStart(), true, true)
	}
}

func TestVODProbeRejectsHarmlessPlaylist(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	// A normal playlist and inert segment on an isolated loopback fixture.
	// The VOD restriction must reject the playlist without reading its child.
	var children atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/video.mkv" {
			children.Add(1)
			w.Write([]byte("unused fixture segment"))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()
	input := srv.URL + "/video.mkv"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobe, probeArgs(input, probeAttempts[0])...).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not on whitelist") {
		t.Fatalf("expected explicit demuxer rejection, got %v: %s", err, out)
	}
	if n := children.Load(); n != 0 {
		t.Fatalf("playlist child read %d times", n)
	}
}
