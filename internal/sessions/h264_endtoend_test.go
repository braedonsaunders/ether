package sessions

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func TestH264ProfileDecisionMatchesFirstSegmentSPS(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	ff, e1 := exec.LookPath("ffmpeg")
	fp, e2 := exec.LookPath("ffprobe")
	if e1 != nil || e2 != nil {
		t.Skip("ffmpeg and ffprobe required")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=size=3840x2160:rate=30", "-f", "lavfi", "-i", "anullsrc=channel_layout=5.1:sample_rate=48000", "-t", "0.5", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:log-level=error", "-c:a", "ac3", "-b:a", "384k", source}
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("source: %v: %s", err, out)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer server.Close()
	m, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "hls"), FastStart: true, FastStartSegments: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	info, err := m.Probe(t.Context(), server.URL+"/source.mp4")
	if err != nil {
		t.Fatal(err)
	}
	profiles := &core.ClientCapabilityProfiles{DirectPlay: []core.ClientDirectPlayProfile{{MediaType: "video", Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"ac3"}}}, Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", Delivery: "hls", Containers: []string{"mpegts"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"ac3"}, MaxAudioChannels: "6"}}}
	caps := core.ClientCaps{Profiles: profiles, Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"ac3"}, HLSContainers: []string{"mpegts"}}
	b := &Broker{deps: BrokerDeps{Transcoder: m}}
	d, err := b.decideProfile(&core.PlaySession{Info: *info}, frontend.PlanRequest{Caps: caps, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: 1, SubtitleStream: -1}}})
	if err != nil || d.VideoCopy || d.H264Level != 51 {
		t.Fatalf("negotiated decision %+v: %v", d, err)
	}
	hs, err := m.StartHLS(t.Context(), core.HLSRequest{SessionID: "profile-e2e", InputURL: server.URL + "/source.mp4", Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	hs.ServeSegment(w, httptest.NewRequest(http.MethodGet, "/00000.ts", nil), 0)
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("segment %d: %s", w.Code, w.Body.String())
	}
	segment := filepath.Join(dir, "first.ts")
	if err := os.WriteFile(segment, w.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(fp, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=profile,level,width,height", "-of", "json", segment).Output()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Streams []struct {
			Profile string `json:"profile"`
			Level   int    `json:"level"`
			Width   int    `json:"width"`
			Height  int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	attributes := transcode.HLSVariantAttributes(*info, d)
	if len(probe.Streams) != 1 || probe.Streams[0].Profile != "High" || probe.Streams[0].Level != 51 || probe.Streams[0].Width != 3840 || probe.Streams[0].Height != 2160 || !strings.Contains(attributes, "avc1.640033") {
		t.Fatalf("segment %s; master attributes %s", out, attributes)
	}
}
