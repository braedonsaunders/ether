package transcode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Exercise the actual session and fast-start segment path, not merely planned
// arguments. Streamyfin's first segment exposed a mismatch with the master.
func TestH264FastStartSegmentMatchesAdvertisedCodecs(t *testing.T) {
	e := newRealEnv(t)
	source := filepath.Join(e.dir, "source.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=size=3840x2160:rate=30", "-t", "0.5", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:log-level=error", source}
	if out, err := exec.Command(e.ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("source encode: %v: %s", err, out)
	}
	info, err := e.m.Probe(t.Context(), e.url("source.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	e.m.UpdateConfig(config.Transcode{FastStart: true, FastStartSegments: 2})
	d := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", Container: ContainerMPEGTS, BitrateKbps: 8000, AudioStream: -1, SubtitleStream: -1, H264Level: 51, H264FrameRate: 30}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "profile-proof", InputURL: e.url("source.mp4"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	hs.ServeSegment(w, httptest.NewRequest(http.MethodGet, "/00000.ts", nil), 0)
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("first segment: %d %s", w.Code, w.Body.String())
	}
	segment := filepath.Join(e.dir, "first.ts")
	if err := os.WriteFile(segment, w.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=profile,level,width,height", "-of", "json", segment).Output()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Streams []struct {
			Profile string `json:"profile"`
			Level   int    `json:"level"`
			Width   int    `json:"width"`
			Height  int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Streams) != 1 || parsed.Streams[0].Profile != "High" || parsed.Streams[0].Level != 51 || parsed.Streams[0].Width != 3840 || parsed.Streams[0].Height != 2160 || !strings.Contains(HLSVariantAttributes(*info, d), "avc1.640033") {
		t.Fatalf("first segment %s; attributes %s", out, HLSVariantAttributes(*info, d))
	}
}
