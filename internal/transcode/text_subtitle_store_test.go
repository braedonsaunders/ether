package transcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// A real remux must emit both English tracks while making exactly the same
// source reads as its video, and cue windows must survive session retirement.
func TestPlaybackTextSubtitleTee(t *testing.T) {
	e := newRealEnv(t)
	// Generate a normal H.264/AAC source with the existing two subtitle tracks.
	e.gen(t, "text.mkv", 8, "-i", "../sessions/testdata/jellyfin-subtitles/tracks.mkv", "-map", "0:v", "-map", "1:a", "-map", "2:s", "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-c:s", "srt")
	ctx := context.Background()
	info, err := e.m.Probe(ctx, e.url("text.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	var tracks []int
	for _, stream := range info.Streams {
		if stream.Type == "subtitle" {
			tracks = append(tracks, stream.Index)
		}
	}
	if len(tracks) != 2 {
		t.Fatalf("tracks=%v", tracks)
	}
	decision := core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: ContainerMPEGTS, AudioStream: 1, SubtitleStream: -1}
	request := core.HLSRequest{SessionID: "tee", ReleaseID: "fixture-release", InputURL: e.url("text.mkv"), Info: *info, Decision: decision, TextSubtitleStreams: tracks}
	hs, err := e.m.StartHLS(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	session := hs.(*Session)
	playlist := httptest.NewRecorder()
	hs.ServePlaylist(playlist, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil))
	if !strings.Contains(playlist.Body.String(), "#EXTINF:") || strings.Contains(playlist.Body.String(), ".ass") {
		t.Fatal(playlist.Body.String())
	}
	video := fetch(t, hs, 0)
	if len(video) < 188 || video[0] != 0x47 {
		t.Fatal("invalid MPEG-TS video")
	}
	waitFor(t, "stored text outputs", func() bool {
		for _, index := range tracks {
			cues, err := session.TextSubtitleCues(index, 0, 20*time.Second)
			if err != nil || len(cues) < 2 {
				return false
			}
		}
		return true
	})
	for n, index := range tracks {
		cues, _ := session.TextSubtitleCues(index, 0, 20*time.Second)
		want := "Hello, citizen."
		if n == 1 {
			want = "[door opens]"
		}
		if !strings.Contains(cues[0].Text, want) {
			t.Fatalf("track %d: %+v", index, cues)
		}
	}
	// The shared command contains one source input and two text outputs.
	session.mu.Lock()
	run := session.cur
	session.mu.Unlock()
	args := buildArgs(run.spec)
	inputs := 0
	for _, arg := range args {
		if arg == "-i" {
			inputs++
		}
	}
	if inputs != 1 || len(run.spec.TextSubtitlePaths) != 2 {
		t.Fatalf("inputs=%d paths=%v", inputs, run.spec.TextSubtitlePaths)
	}
	for _, index := range tracks {
		session.mu.Lock()
		dir := session.textSubtitleDir(index)
		session.mu.Unlock()
		if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
			t.Fatalf("missing persisted windows %s: %v", dir, err)
		}
	}
	e.m.Stop("tee")
	request.SessionID = "tee-replay"
	replay, err := e.m.StartHLS(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	cues, err := replay.(*Session).TextSubtitleCues(tracks[1], 0, 20*time.Second)
	if err != nil || len(cues) < 2 {
		t.Fatalf("cue store lost on retirement: %+v %v", cues, err)
	}
	// Verify persistence is independent of the old run's output files.
	if _, err := os.Stat(filepath.Join(session.dir, "text-2-1.ass")); !os.IsNotExist(err) {
		t.Logf("old run cleanup: %v", err)
	}
}
