//go:build transcodechaos

package plex

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type chaosByteWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (w chaosByteWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n.Add(int64(n))
	return n, err
}

// Stretch 60 seconds of generated packets across an 80-minute clock. This
// tests a 70-minute range seek without generating an 80-minute encode.
func TestTranscodeChaosPCELongSeek(t *testing.T) {
	ff, fp := os.Getenv("CHAOS_FFMPEG"), os.Getenv("CHAOS_FFPROBE")
	if ff == "" || fp == "" {
		t.Skip("requires pinned ffmpeg")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "pce-long.ts")
	chaosCmd(t, ff, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=12:duration=60", "-f", "lavfi", "-i", "anullsrc=channel_layout=5.1(side):sample_rate=48000:duration=60", "-filter_complex", "[0:v]setpts=80*PTS[v];[1:a]asetpts=80*PTS[a]", "-map", "[v]", "-map", "[a]", "-fps_mode", "passthrough", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "1", "-b:v", "2M", "-c:a", "aac", "-channel_layout:a", "5.1(side)", "-f", "mpegts", file)
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	var count atomic.Int64
	var historyMu sync.Mutex
	var history []string
	files := http.FileServer(http.Dir(dir))
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestBytes atomic.Int64
		files.ServeHTTP(chaosByteWriter{w, &requestBytes}, r)
		n := requestBytes.Load()
		count.Add(n)
		historyMu.Lock()
		history = append(history, r.Header.Get("Range")+": "+strconv.FormatInt(n, 10))
		historyMu.Unlock()
	}))
	defer source.Close()
	m, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "work")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithSegmentTimeout(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	url := source.URL + "/pce-long.ts"
	info, err := m.Probe(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	pce := false
	for _, a := range info.Streams {
		if a.Type == "audio" && a.AACProgramConfig && a.AACConfigPTS != nil && a.AACSourceStart != nil {
			pce = true
		}
	}
	if !pce {
		t.Fatal("probe did not retain the PCE source timestamps")
	}
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = *info
	f.srv.setPlayback(&chaosPlayback{f.playback, url})
	f.srv.setTranscoder(m)
	count.Store(0)
	historyMu.Lock()
	history = nil
	historyMu.Unlock()
	start := f.get(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F"+fightClub.Key.String()+"&session=pce-late&protocol=hls&directPlay=0&directStream=1&offset=4200", "X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome")
	if start.Code != http.StatusOK {
		t.Fatalf("late seek start HTTP %d", start.Code)
	}
	media := f.get(t, "/video/:/transcode/universal/session/pce-late/base/00000.ts", "X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome")
	if media.Code != http.StatusOK || media.Body.Len() == 0 {
		t.Fatalf("late seek segment HTTP %d", media.Code)
	}
	out := filepath.Join(dir, "late.ts")
	if err := os.WriteFile(out, media.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	chaosCmd(t, ff, "-v", "error", "-xerror", "-i", out, "-f", "null", "-")
	packets, err := mediaproc.CommandContext(context.Background(), 2*time.Minute, fp, "-v", "error", "-select_streams", "a:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", out).Output()
	if err != nil || strings.TrimSpace(string(packets)) == "" {
		t.Fatal("late seek segment has no audio")
	}
	video, err := mediaproc.CommandContext(context.Background(), 2*time.Minute, fp, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", out).Output()
	if err != nil || strings.TrimSpace(string(video)) == "" {
		t.Fatal("late seek segment has no video")
	}
	firstPTS := func(data []byte) float64 {
		for _, line := range strings.Split(string(data), "\n") {
			value, err := strconv.ParseFloat(strings.TrimSpace(strings.Split(line, ",")[0]), 64)
			if err == nil {
				return value
			}
		}
		return -1
	}
	videoPTS, audioPTS := firstPTS(video), firstPTS(packets)
	if videoPTS < 4192 || videoPTS > 4208 || audioPTS < 4192 || audioPTS > 4208 {
		t.Fatalf("seek output has wrong clock: video %.3f audio %.3f", videoPTS, audioPTS)
	}
	t.Logf("first packet timestamps: video %.3f audio %.3f", videoPTS, audioPTS)
	f.get(t, "/video/:/transcode/universal/stop?session=pce-late", "X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome")
	read := count.Load()
	t.Logf("70-minute seek: source bytes served %d of %d", read, st.Size())
	historyMu.Lock()
	t.Logf("source ranges after probing: %v", history)
	historyMu.Unlock()
	if read >= st.Size()/2 {
		t.Fatalf("late seek read too much source data: %d of %d", read, st.Size())
	}
}
