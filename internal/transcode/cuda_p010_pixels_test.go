package transcode

import (
	"bytes"
	"os/exec"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// This checks decoded P010 samples when a local CUDA device is available.
// It is not a throughput or display-colour validation.
func TestCUDAP010DecodedPixels(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, ff, cudaP010ProbeArgs()...).CombinedOutput(); err != nil {
		t.Skipf("CUDA P010 tone-map unavailable on this host: %v (%s)", err, out)
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=1:duration=1,format=p010le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc", "-vf", "hwupload_cuda,tonemap_cuda=tonemap=hable:format=p010le:t=bt709:m=bt709:p=bt709,hwdownload,format=p010le", "-frames:v", "1", "-f", "rawvideo", "pipe:1"}
	cmd := mediaproc.CommandContext(t.Context(), 2*time.Minute, ff, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pixels, err := cmd.Output()
	if err != nil {
		t.Fatalf("P010 frame: %v: %s", err, stderr.String())
	}
	if len(pixels) != 64*64*3 {
		t.Fatalf("P010 frame size = %d", len(pixels))
	}
	if bytes.Count(pixels, []byte{0}) == len(pixels) {
		t.Fatal("tone mapped frame is black")
	}
}
