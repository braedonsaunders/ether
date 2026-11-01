package playback

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type realTrackSource struct {
	*os.File
	size int64
	id   string
}

func (s *realTrackSource) Size() int64         { return s.size }
func (s *realTrackSource) ReleaseID() string   { return s.id }
func (s *realTrackSource) LocalFile() *os.File { return s.File }

type realTrackEngine struct{ path string }

func (e *realTrackEngine) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	if id != "track-source" && id != "alternate" {
		return nil, core.ErrNotFound
	}
	f, err := os.Open(e.path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &realTrackSource{File: f, size: st.Size(), id: id}, nil
}
func (*realTrackEngine) Prefetch(context.Context, string, []byte, int64, int64) error {
	return nil
}

// This offline integration uses a real generated file and FFmpeg/ffprobe; the
// broker and playback manager are real while identity and catalogue are fakes.
func TestSourceTrackSelectionOnRealOpenedMedia(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg and ffprobe")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()
	subtitle := filepath.Join(dir, "subtitle.srt")
	if err := os.WriteFile(subtitle, []byte("1\n00:00:00,000 --> 00:00:01,500\nfixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "source.mkv")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=size=160x90:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-i", subtitle, "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000", "-map", "0:v:0", "-map", "1:a:0", "-map", "2:s:0", "-map", "3:a:0", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-c:s", "srt", path}
	if output, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v: %s", err, output)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "hls"), FastStart: true, FastStartSegments: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	// Verify the alternate really can be opened and selected without a pin;
	// otherwise a failed failover would not demonstrate pin enforcement.
	control := newHarness(t, nil, []string{"track-source", "alternate"}, func(h *harness, _ *Options) {
		h.testEngine, h.testTranscode = &realTrackEngine{path: path}, tc
	})
	controlCaps := defaultCaps("control")
	controlCaps.Protocol = "silo"
	controlSource, err := control.m.PrepareSurface(ctx, friend, movieKey, controlCaps, "", false)
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := control.m.Failover(ctx, controlSource.ID, 0)
	if err != nil || alternate == nil || alternate.Resolution == nil || alternate.Resolution.Release.ID != "alternate" {
		t.Fatalf("unpinned alternate unavailable: %+v %v", alternate, err)
	}
	control.m.Stop(controlSource.ID)
	h := newHarness(t, nil, []string{"track-source", "alternate"}, func(h *harness, _ *Options) {
		h.testEngine, h.testTranscode = &realTrackEngine{path: path}, tc
	})
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	lookup := func(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return h.catalog.Get(ctx, key)
	}
	var selected core.DecideOptions
	b, err := sessions.NewBroker(sessions.BrokerDeps{DB: h.m.resume.db, Events: events, Playback: h.m, Transcoder: tc, Progress: h.state, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup, SelectScopedTracks: func(_ context.Context, _ *core.Viewer, _ core.Device, _ *core.Title, _ core.MediaInfo, opts core.DecideOptions, audioSet, subtitleSet bool) (core.DecideOptions, error) {
		if !audioSet || !subtitleSet {
			return opts, core.ErrNotAvailable
		}
		selected = opts
		return opts, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	device := core.Device{ID: "silo-track-device"}
	caps := defaultCaps(device.ID)
	caps.HLSContainers = []string{"mpegts"}
	caps.SubtitleCodecs = []string{"srt"}
	audioOrdinal, subtitleIndex := 1, 2
	req := frontend.PlanRequest{Viewer: friend, Device: device, Protocol: "silo", Item: movieKey, Caps: caps, Intent: frontend.PlayIntent{AudioOrdinal: &audioOrdinal, SubtitleStreamIndex: &subtitleIndex, Options: core.DecideOptions{DirectStream: true, AudioStream: -1, SubtitleStream: -1}}}
	plan, err := b.Plan(ctx, req)
	if err != nil || plan.Media.Estimated || selected.AudioStream != 3 || selected.SubtitleStream != 2 || plan.Decision.AudioStream != 3 || plan.Decision.SubtitleStream != 2 {
		t.Fatalf("selection plan=%+v selected=%+v err=%v", plan, selected, err)
	}
	if len(plan.Media.Streams) != 4 || plan.Media.Streams[0].Type != "video" || plan.Media.Streams[1].Type != "audio" || plan.Media.Streams[1].Index != 1 || plan.Media.Streams[2].Type != "subtitle" || plan.Media.Streams[2].Index != 2 || plan.Media.Streams[3].Type != "audio" || plan.Media.Streams[3].Index != 3 {
		t.Fatalf("unexpected probed stream order: %+v", plan.Media.Streams)
	}
	bad := req
	badIndex := 99
	bad.Intent.SubtitleStreamIndex = &badIndex
	if _, err := b.Plan(ctx, bad); !errors.Is(err, frontend.ErrTrackNotFound) {
		t.Fatalf("foreign subtitle index: %v", err)
	}
	sources := h.m.Sessions()
	if len(sources) != 1 || sources[0].Source == nil || sources[0].Source.ReleaseID() != "track-source" {
		t.Fatalf("sources %+v", sources)
	}
	if _, err := h.m.Failover(ctx, sources[0].ID, 0); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("track-selected generation was not pinned: %v", err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: friend, Device: device, Protocol: "silo", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	master, err := b.StartStream(ctx, frontend.SessionAccess{Viewer: friend, Device: device, Protocol: "silo"}, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		b.ServeMedia(w, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		return w
	}
	base := strings.TrimSuffix(master, "master.m3u8") + "base/"
	playlist := fetch(base + "index.m3u8")
	if playlist.Code != http.StatusOK {
		t.Fatalf("playlist status %d", playlist.Code)
	}
	segmentName := ""
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if strings.HasSuffix(line, ".ts") && !strings.ContainsAny(line, "/\\") {
			segmentName = line
			break
		}
	}
	if segmentName == "" {
		t.Fatalf("no TS segment in %q", playlist.Body.String())
	}
	segment := fetch(base + segmentName)
	if segment.Code != http.StatusOK || segment.Body.Len() == 0 {
		t.Fatalf("segment status %d", segment.Code)
	}
	segmentPath := filepath.Join(dir, "segment.ts")
	if err := os.WriteFile(segmentPath, segment.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	pcm, err := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-xerror", "-i", segmentPath, "-map", "0:a:0", "-ac", "1", "-ar", "8000", "-f", "s16le", "-").Output()
	if err != nil || len(pcm) < 1600 {
		t.Fatalf("decode selected audio: bytes=%d err=%v", len(pcm), err)
	}
	// The two generated tracks are 440 Hz and 880 Hz. A zero-crossing check
	// verifies actual output came from the second source stream.
	crossings := 0
	previous := int16(binary.LittleEndian.Uint16(pcm[:2]))
	for pos := 2; pos+1 < len(pcm); pos += 2 {
		value := int16(binary.LittleEndian.Uint16(pcm[pos : pos+2]))
		if (previous < 0 && value >= 0) || (previous >= 0 && value < 0) {
			crossings++
		}
		previous = value
	}
	frequency := float64(crossings) * 8000 / (2 * float64(len(pcm)/2))
	if frequency < 750 || frequency > 1000 {
		t.Fatalf("wrong selected audio frequency %.1f Hz", frequency)
	}
	skipOpenFileReplaceOnWindows(t)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if changed := fetch(base + segmentName); changed.Code != http.StatusNotFound {
		t.Fatalf("changed source status %d", changed.Code)
	}
}
