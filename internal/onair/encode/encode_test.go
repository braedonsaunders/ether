package encode

import (
	"context"
	"errors"
	"image"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// A real subprocess that never reads input exercises cancellation of a blocked
// rawvideo pipe, worker teardown and reaping, without requiring installed ffmpeg.
func TestMain(m *testing.M) {
	if os.Getenv("ONAIR_ENCODE_HELPER") == "1" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestCancellation(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip("subprocess unavailable")
	}
	bin, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("ONAIR_ENCODE_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	e = WithWorkers(ctx, bin, func(int) *image.RGBA { return image.NewRGBA(image.Rect(0, 0, 1920, 1080)) }, 120, 30, "", t.TempDir()+"/clip.mp4", 2)
	if e == nil {
		t.Fatal("cancellation succeeded unexpectedly")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("slow cancellation")
	}
}

func TestLongPluginMasterKeepsDeclaredDuration(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	still := image.NewRGBA(image.Rect(0, 0, 64, 36))
	out := t.TempDir() + "/forecast.mp4"
	if err := WithOptions(ctx, ff, func(int) *image.RGBA { return still }, 120*30, 30, "", out, Options{Workers: 1, Width: 64, Height: 36, MaxSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	b, err := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil || duration < 120 || duration > 120.1 {
		t.Fatalf("plugin duration %s: %v", b, err)
	}
}
