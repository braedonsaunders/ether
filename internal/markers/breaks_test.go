package markers

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestBreakSignalsAndChapters(t *testing.T) {
	s := breakSignals{black: []breakRun{{70, 71}, {130, 131}, {150, 155}}, silent: []breakRun{{69.9, 71.1}, {149, 156}}, scenes: []float64{70}}
	got := combineBreakSignals(s, 3*time.Minute)
	if len(got) != 1 || got[0].AtMS != 70500 || got[0].Confidence != .96 {
		t.Fatalf("black+silence vs dialogue/long dark trap: %+v", got)
	}
	var parsed breakSignals
	start := -1.0
	for _, line := range []string{"silence_start: 69.9", "black_start:70 black_end:71 black_duration:1", "lavfi.scd.time: 70", "silence_end: 71.1 | silence_duration: 1.2"} {
		parsed.line(line, &start)
	}
	if got = combineBreakSignals(parsed, 3*time.Minute); len(got) != 1 {
		t.Fatal(got)
	}
	info := core.MediaInfo{Duration: 3 * time.Minute, Chapters: []core.Chapter{{Title: "Act 1", Start: 0}, {Title: "Act 2", Start: 70 * time.Second}, {Title: "Chapter 3", Start: 90 * time.Second}, {Title: "Credits", Start: 110 * time.Second}}}
	if got = ChapterBreaks(info); len(got) != 1 || got[0].AtMS != 70000 || got[0].Source != "chapters" {
		t.Fatal(got)
	}
}

func TestKBestBreakSpacing(t *testing.T) {
	points := []BreakPoint{{420000, "detected", .91}, {430000, "detected", .96}, {900000, "chapters", .99}, {20000, "detected", 1}, {1100000, "detected", .89}}
	got := SelectBreaks(points, 1320000, 2, 180000)
	if len(got) != 2 || got[0].AtMS != 430000 || got[1].AtMS != 900000 {
		t.Fatal(got)
	}
	if len(SelectBreaks(points, 1320000, 2, 1000000)) != 1 {
		t.Fatal("minimum spacing ignored")
	}
}

func TestBreakPersistenceReuseAndRevision(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := BreakAnalyzer{DB: db}
	in := BreakInput{ReleaseKey: "release-a-payload-file", Info: core.MediaInfo{Duration: 3 * time.Minute, Chapters: []core.Chapter{{Title: "Act 2", Start: 70 * time.Second}}}}
	got, err := a.Analyze(t.Context(), in)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	in.Info.Chapters = nil
	if got, err = a.Analyze(t.Context(), in); err != nil || len(got) != 1 {
		t.Fatal("cache did not bypass decoder", got, err)
	}
	in.ReleaseKey = "release-b"
	if _, err = a.Analyze(t.Context(), in); err == nil {
		t.Fatal("reused another release")
	}
	// Completed empty detection results also bypass decoding.
	_, err = db.Exec(`INSERT INTO markers_breaks_cache VALUES(?,?,?,?,?)`, "empty", breakVersion, 180000, "[]", 0)
	if err != nil {
		t.Fatal(err)
	}
	in.ReleaseKey = "empty"
	if got, err = a.Analyze(t.Context(), in); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}

// Optional end-to-end fixture, using a caller-supplied verified ffmpeg binary.
// No provider or network access; ordinary CI exercises the signal invariants above.
func TestBreakSyntheticDecode(t *testing.T) {
	binary := os.Getenv("ETHER_BREAK_TEST_FFMPEG")
	if binary == "" {
		t.Skip("set ETHER_BREAK_TEST_FFMPEG for generated media test")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "synthetic.mkv")
	cmd := exec.Command(binary, "-v", "error", "-nostdin", "-threads", "1", "-filter_threads", "1", "-f", "lavfi", "-i", "color=white:s=160x90:r=10:d=180", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000:duration=180", "-vf", "drawbox=color=black:t=fill:enable='between(t,70,70.9)+between(t,130,130.9)'", "-af", "volume='if(between(t,70,71),0,1)':eval=frame", "-c:v", "ffv1", "-c:a", "pcm_s16le", "-threads", "1", file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", out, err)
	}
	a := BreakAnalyzer{FFmpeg: binary, OnCost: func(video, cpu, wall time.Duration) {
		t.Logf("decode CPU/video-hour=%s wall/video-hour=%s", cpu*(time.Hour/video), wall*(time.Hour/video))
	}}
	started := time.Now()
	got, err := a.decode(t.Context(), BreakInput{URL: file, Info: core.MediaInfo{Duration: 3 * time.Minute}})
	if err != nil || len(got) != 1 || got[0].AtMS < 70000 || got[0].AtMS > 71000 {
		t.Fatal(got, err)
	}
	b, _ := json.Marshal(got)
	if artifacts := os.Getenv("ETHER_BREAK_TEST_ARTIFACTS"); artifacts != "" {
		if err := os.MkdirAll(artifacts, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifacts, "synthetic-timeline.json"), b, 0644); err != nil {
			t.Fatal(err)
		}
		for _, at := range []string{"69", "70.5", "72"} {
			cmd := exec.Command(binary, "-v", "error", "-nostdin", "-threads", "1", "-ss", at, "-i", file, "-frames:v", "1", "-threads", "1", filepath.Join(artifacts, "synthetic-"+at+"s.png"))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("still: %v %s", err, output)
			}
		}
	}
	t.Logf("timeline=%s wall=%s per-video-hour=%s", b, time.Since(started), time.Since(started)*20)
}
