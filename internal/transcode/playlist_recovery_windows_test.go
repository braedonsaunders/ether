package transcode

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// Force the real Windows muxer's destination-replacement failure, not
// merely a fabricated leftover .tmp. The encoder still finishes all media.
func TestIntegrationWindowsPlaylistReplacementRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (-short)")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	playlist := filepath.Join(dir, "run.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(playlist)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := mediaproc.CommandContext(ctx, 2*time.Minute, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24", "-t", "4",
		"-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-g", "24", "-flags", "+global_header",
		"-f", "ssegment", "-segment_format", "matroska", "-segment_time", "1",
		"-segment_list_type", "m3u8", "-segment_list_size", "0", "-segment_list", playlist,
		filepath.Join(dir, "%05d.mkv"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("muxer failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("failed to rename")) {
		t.Fatalf("did not reproduce the muxer replacement failure: %s", out)
	}
	live, err := readFFmpegPlaylist(playlist, false)
	if err != nil || len(live.entries) != 0 {
		t.Fatalf("published playlist should still be empty: %+v, %v", live, err)
	}
	final, err := readFFmpegPlaylist(playlist, true)
	if err != nil || !final.ended || len(final.entries) != 4 {
		t.Fatalf("finished media was lost after failed rename: %+v, %v\n%s", final, err, out)
	}
	for _, e := range final.entries {
		st, err := os.Stat(filepath.Join(dir, e.name))
		if err != nil || st.Size() == 0 {
			t.Fatalf("recovered playlist references missing media %s: %v", e.name, err)
		}
	}
}
