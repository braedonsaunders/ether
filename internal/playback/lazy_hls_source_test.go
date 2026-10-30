package playback

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type hlsFixtureSource struct {
	*os.File
	size int64
}

func (s *hlsFixtureSource) Size() int64         { return s.size }
func (s *hlsFixtureSource) ReleaseID() string   { return "synthetic-source" }
func (s *hlsFixtureSource) LocalFile() *os.File { return s.File }

type hlsFixtureEngine struct {
	path  string
	opens atomic.Int64
}

func (e *hlsFixtureEngine) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	if id != "synthetic-source" {
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
	e.opens.Add(1)
	return &hlsFixtureSource{File: f, size: st.Size()}, nil
}
func (*hlsFixtureEngine) Prefetch(context.Context, string, []byte, int64, int64) error {
	return nil
}

type countedHLSFixtureTranscoder struct {
	core.Transcoder
	probes atomic.Int64
}

func (c *countedHLSFixtureTranscoder) Probe(ctx context.Context, input string) (*core.MediaInfo, error) {
	c.probes.Add(1)
	return c.Transcoder.Probe(ctx, input)
}

type hlsFixtureGrants struct{ revoked atomic.Bool }

func (g *hlsFixtureGrants) GrantReference(_ context.Context, surface, credential string) (string, *core.Viewer, error) {
	if surface != "jellyfin" || credential != "account-secret" || g.revoked.Load() {
		return "", nil, core.ErrUnauthorized
	}
	return "private-reference", &core.Viewer{ID: friend.ID}, nil
}
func (g *hlsFixtureGrants) ValidateGrantReference(_ context.Context, surface, reference string, viewer int64) error {
	if g.revoked.Load() || surface != "jellyfin" || reference != "private-reference" || viewer != friend.ID {
		return core.ErrUnauthorized
	}
	return nil
}

// Real local media exercises broker source pinning and actual HLS bytes. Unit
// tests in sessions remain fake-backed; this integration is entirely offline.
func TestLazyHLSUsesResolvedSourceAndRealSegments(t *testing.T) {
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
	path := filepath.Join(dir, "source.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=size=320x180:rate=24", "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", path}
	if output, err := exec.CommandContext(ctx, ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v: %s", err, output)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256(bytes)
	tc, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "hls"), FastStart: true, FastStartSegments: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	counted := &countedHLSFixtureTranscoder{Transcoder: tc}
	engine := &hlsFixtureEngine{path: path}
	h := newHarness(t, nil, []string{"synthetic-source"}, func(h *harness, _ *Options) {
		h.testEngine, h.testTranscode = engine, counted
	})
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	lookup := func(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return h.catalog.Get(ctx, key)
	}
	b, err := sessions.NewBroker(sessions.BrokerDeps{DB: h.m.resume.db, Events: events, Playback: h.m, Transcoder: counted, Progress: h.state, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	device := core.Device{ID: "fixture-device"}
	caps := defaultCaps(device.ID)
	caps.HLSContainers = []string{"mpegts"}
	req := frontend.PlanRequest{Viewer: friend, Device: device, Protocol: "jellyfin", Item: movieKey, Caps: caps, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}}
	detail, err := b.DetailPlan(ctx, req)
	if err != nil || detail.ETag == "" || detail.DetailSource == "" || !detail.HLS || detail.Media.Estimated {
		t.Fatalf("resolved detail %+v: %v", detail, err)
	}
	if counted.probes.Load() != 1 || engine.opens.Load() != 1 {
		t.Fatalf("detail probes=%d opens=%d", counted.probes.Load(), engine.opens.Load())
	}
	req.DetailSource, req.ExpectedSourceETag = detail.DetailSource, detail.ETag
	grants := &hlsFixtureGrants{}
	lazy, err := sessions.NewLazyStreams(h.m.resume.db, b, req.Protocol, grants, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	issuer, ok := lazy.(frontend.LazyHLSStreams)
	if !ok {
		t.Fatal("missing lazy HLS issuer")
	}
	masterPath, err := issuer.IssueHLS(ctx, "account-secret", req, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil || strings.Contains(masterPath, "account-secret") || strings.Contains(masterPath, "private-reference") {
		t.Fatalf("master path %q: %v", masterPath, err)
	}
	fetch := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		lazy.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
		return w
	}
	master := fetch(masterPath)
	if master.Code != http.StatusOK || !strings.Contains(master.Body.String(), "base/index.m3u8") || strings.Contains(master.Body.String(), "/sessions/media/") || strings.Contains(master.Body.String(), "account-secret") {
		t.Fatalf("master %d %q", master.Code, master.Body.String())
	}
	base := strings.TrimSuffix(masterPath, "master.m3u8")
	playlist := fetch(base + "base/index.m3u8")
	if playlist.Code != http.StatusOK || strings.Contains(playlist.Body.String(), "/sessions/media/") || strings.Contains(playlist.Body.String(), "account-secret") {
		t.Fatalf("playlist %d %q", playlist.Code, playlist.Body.String())
	}
	segmentName := ""
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if strings.HasSuffix(line, ".ts") && !strings.ContainsAny(line, "/\\") {
			segmentName = line
			break
		}
	}
	if segmentName == "" {
		t.Fatalf("no relative TS segment in %q", playlist.Body.String())
	}
	segment := fetch(base + "base/" + segmentName)
	if segment.Code != http.StatusOK || segment.Body.Len() == 0 {
		t.Fatalf("segment status %d", segment.Code)
	}
	segmentPath := filepath.Join(dir, "segment.ts")
	if err := os.WriteFile(segmentPath, segment.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-xerror", "-i", segmentPath, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("decode segment: %v: %s", err, output)
	}
	sources := h.m.Sessions()
	if len(sources) != 1 || sources[0].Source == nil || sources[0].Source.ReleaseID() != "synthetic-source" || core.SourceRevision(sources[0]) != detail.ETag {
		t.Fatalf("wrong opened source: %+v", sources)
	}
	read := make([]byte, len(bytes))
	if _, err := sources[0].Source.ReadAt(read, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if sha256.Sum256(read) != expectedHash || counted.probes.Load() != 1 || engine.opens.Load() != 1 {
		t.Fatalf("source mismatch or repeated probe: probes=%d opens=%d", counted.probes.Load(), engine.opens.Load())
	}
	grants.revoked.Store(true)
	if denied := fetch(base + "base/" + segmentName); denied.Code != http.StatusNotFound {
		t.Fatalf("revoked segment status %d", denied.Code)
	}
	grants.revoked.Store(false)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	if changed := fetch(base + "base/" + segmentName); changed.Code != http.StatusNotFound {
		t.Fatalf("changed source status %d", changed.Code)
	}
}
