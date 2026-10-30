package playback

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type consumerRealHLSTranscoder struct {
	*countedHLSFixtureTranscoder
	mu       sync.Mutex
	requests []core.HLSRequest
}

func (c *consumerRealHLSTranscoder) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	c.mu.Lock()
	c.requests = append(c.requests, r)
	c.mu.Unlock()
	return c.Transcoder.StartHLS(ctx, r)
}
func (c *consumerRealHLSTranscoder) starts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}
func (c *consumerRealHLSTranscoder) last() core.HLSRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return core.HLSRequest{}
	}
	return c.requests[len(c.requests)-1]
}

type consumerRealHLSGrants struct {
	hlsFixtureGrants
	protocol string
}

func (g *consumerRealHLSGrants) GrantReference(ctx context.Context, surface, credential string) (string, *core.Viewer, error) {
	if surface != g.protocol {
		return "", nil, core.ErrUnauthorized
	}
	return g.hlsFixtureGrants.GrantReference(ctx, "jellyfin", credential)
}
func (g *consumerRealHLSGrants) ValidateGrantReference(ctx context.Context, surface, ref string, viewer int64) error {
	if surface != g.protocol {
		return core.ErrUnauthorized
	}
	return g.hlsFixtureGrants.ValidateGrantReference(ctx, "jellyfin", ref, viewer)
}
func consumerHasBox(data []byte, kind string) bool {
	for len(data) >= 8 {
		n := uint64(binary.BigEndian.Uint32(data[:4]))
		header := uint64(8)
		if n == 1 {
			if len(data) < 16 {
				return false
			}
			n = binary.BigEndian.Uint64(data[8:16])
			header = 16
		}
		if n == 0 {
			n = uint64(len(data))
		}
		if n < header || n > uint64(len(data)) {
			return false
		}
		if string(data[4:8]) == kind {
			return true
		}
		data = data[n:]
	}
	return false
}
func TestMediaBrowserLazyHLSRealSegments(t *testing.T) {
	for _, protocol := range []string{"jellyfin", "emby"} {
		for _, container := range []string{"mpegts", "fmp4"} {
			t.Run(protocol+"/"+container, func(t *testing.T) { runMediaBrowserLazyHLSRealSegments(t, protocol, container) })
		}
	}
}

func runMediaBrowserLazyHLSRealSegments(t *testing.T, protocol, container string) {
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
	sourceBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256(sourceBytes)
	tc, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, HWAccel: "none", TempDir: filepath.Join(dir, "hls"), FastStart: true, FastStartSegments: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tc.Close(); err != nil {
			t.Error("encoder cleanup", err)
		}
	}()
	counted := &consumerRealHLSTranscoder{countedHLSFixtureTranscoder: &countedHLSFixtureTranscoder{Transcoder: tc}}
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
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		b.Close(cleanupCtx)
		if len(h.m.Sessions()) != 0 {
			t.Error("playback sources remain after broker cleanup")
		}
		if id := counted.last().SessionID; id != "" {
			if _, ok := tc.Session(id); ok {
				t.Error("encoder session remains after broker cleanup")
			}
		}
	}()
	device := core.Device{ID: "fixture-device"}
	caps := defaultCaps(device.ID)

	caps.Product, caps.Protocol = "offline-consumer", frontend.Protocol(protocol)
	caps.HLSContainers = []string{container}
	if container == "fmp4" {
		// Explicitly reject direct play so the broker chooses an eligible fMP4
		// copy/remux profile rather than the default direct-to-TS HLS conversion.
		caps.Profiles = &core.ClientCapabilityProfiles{
			DirectPlay:  []core.ClientDirectPlayProfile{},
			Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", Delivery: "hls", Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}},
		}
	}
	grants := &consumerRealHLSGrants{protocol: protocol}
	lazy, err := sessions.NewLazyStreams(h.m.resume.db, b, frontend.Protocol(protocol), grants, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	host := frontend.Host{
		Sessions: sessions.BindSurface(b, frontend.Protocol(protocol)), LazyStreams: lazy,
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://fixture.invalid", nil },
		Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*core.Viewer, error) {
			if token != "account-secret" {
				return nil, core.ErrUnauthorized
			}
			return friend, nil
		}},
		Experience: frontendtest.Experience{ItemFunc: func(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			title, err := h.catalog.Get(ctx, movieKey)
			if err != nil {
				return frontend.ItemDetail{}, err
			}
			return frontend.ItemDetail{Title: *title}, nil
		}},
		Capabilities: frontendtest.Capabilities{BuildFunc: func(context.Context, frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
			return frontend.CapabilityResult{Client: caps}, nil
		}},
	}
	var front frontend.Frontend = mediabrowser.Jellyfin{MachineID: "offline"}
	if protocol == "emby" {
		front = mediabrowser.Emby{MachineID: "offline"}
	}
	inst, err := front.Mount(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil).WithContext(ctx)
		if authenticated {
			r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="fixture-device"`)
		}
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	response := request(http.MethodGet, "/Items/"+ids.RatingKeyGUID(movieKey), true)
	var dto struct {
		MediaSources []struct {
			Id, ETag, TranscodingUrl, TranscodingContainer string
			SupportsDirectPlay                             bool
		}
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &dto) != nil || len(dto.MediaSources) != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	offered := dto.MediaSources[0]
	wireContainer := "ts"
	if container == "fmp4" {
		wireContainer = "mp4"
	}
	if offered.TranscodingContainer != wireContainer || !strings.HasPrefix(offered.TranscodingUrl, "https://fixture.invalid/sessions/lazy/") || strings.Contains(response.Body.String(), "account-secret") || strings.Contains(response.Body.String(), "private-reference") {
		t.Fatal("invalid detail offer", response.Body.String())
	}
	scope := frontend.VersionScope{Viewer: friend, Device: device, Protocol: frontend.Protocol(protocol), Caps: caps}
	target, err := b.ResolveDetailSource(ctx, scope, offered.Id)
	if err != nil || target.Item != movieKey || target.ETag == "" {
		t.Fatal("unbound detail", target, err)
	}
	if protocol == "jellyfin" && offered.ETag != target.ETag {
		t.Fatal("DTO revision differs", offered.ETag, target.ETag)
	}
	for _, foreign := range []frontend.VersionScope{
		{Viewer: friend, Device: core.Device{ID: "other"}, Protocol: scope.Protocol, Caps: caps},
		{Viewer: &core.Viewer{ID: friend.ID + 1}, Device: device, Protocol: scope.Protocol, Caps: caps},
		{Viewer: friend, Device: device, Protocol: "other", Caps: caps},
	} {
		if _, err := b.ResolveDetailSource(ctx, foreign, offered.Id); err == nil {
			t.Fatal("foreign source handle accepted")
		}
	}
	if counted.probes.Load() != 1 || engine.opens.Load() != 1 || counted.starts() != 0 {
		t.Fatal("detail did not remain lazy", counted.starts())
	}
	masterPath := strings.TrimPrefix(offered.TranscodingUrl, "https://fixture.invalid")
	if head := request(http.MethodHead, masterPath, false); head.Code != http.StatusOK || counted.starts() != 0 {
		t.Fatal("HEAD started playback", head.Code)
	}
	fetch := func(path string) *httptest.ResponseRecorder { return request(http.MethodGet, path, false) }
	master := fetch(masterPath)
	if master.Code != http.StatusOK || !strings.Contains(master.Body.String(), "base/index.m3u8") || strings.Contains(master.Body.String(), "/sessions/media/") || strings.Contains(master.Body.String(), "account-secret") {
		t.Fatalf("master %d %q", master.Code, master.Body.String())
	}
	base := strings.TrimSuffix(masterPath, "master.m3u8")
	playlist := fetch(base + "base/index.m3u8")
	if playlist.Code != http.StatusOK || strings.Contains(playlist.Body.String(), "/sessions/media/") || strings.Contains(playlist.Body.String(), "account-secret") {
		t.Fatalf("playlist %d %q", playlist.Code, playlist.Body.String())
	}

	selected := counted.last()
	if counted.starts() != 1 || selected.Decision.Container != container || selected.Protocol != core.ProtocolHLS || selected.Decision.Kind != core.DecisionRemux || !selected.Decision.VideoCopy || !selected.Decision.AudioCopy {
		t.Fatalf("DTO %q vs encoder %+v starts=%d", offered.TranscodingContainer, selected, counted.starts())
	}
	if err := b.MatchDetailSession(ctx, scope, offered.Id, selected.SessionID); err != nil {
		t.Fatal("opened session lost scoped source binding", err)
	}
	if container == "mpegts" && !offered.SupportsDirectPlay {
		t.Fatal("direct-to-remux case was not directly playable")
	}
	segmentName, initName := "", ""
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if strings.HasPrefix(line, "#EXT-X-MAP:URI=") {
			initName = strings.Trim(strings.TrimPrefix(line, "#EXT-X-MAP:URI="), "\"")
		}
		if !strings.HasPrefix(line, "#") && line != "" && segmentName == "" {
			segmentName = line
		}
	}
	safeName := func(name string) bool {
		return name != "" && !strings.ContainsAny(name, "/\\?#") && name != "." && name != ".."
	}
	if !safeName(segmentName) {
		t.Fatal("unsafe segment", segmentName)
	}
	segment := fetch(base + "base/" + segmentName)
	if segment.Code != http.StatusOK || segment.Body.Len() == 0 {
		t.Fatal("segment", segment.Code)
	}
	mediaBytes := segment.Body.Bytes()
	if container == "fmp4" {
		if !safeName(initName) || !strings.HasSuffix(segmentName, ".m4s") {
			t.Fatal("missing fMP4 init/segment", playlist.Body.String())
		}
		init := fetch(base + "base/" + initName)
		if init.Code != http.StatusOK || !consumerHasBox(init.Body.Bytes(), "ftyp") || !consumerHasBox(init.Body.Bytes(), "moov") || !consumerHasBox(mediaBytes, "moof") || !consumerHasBox(mediaBytes, "mdat") {
			t.Fatal("invalid fMP4 boxes", init.Code)
		}
		mediaBytes = append(append([]byte{}, init.Body.Bytes()...), mediaBytes...)
	} else if initName != "" || !strings.HasSuffix(segmentName, ".ts") {
		t.Fatal("TS metadata differs from playlist", playlist.Body.String())
	}
	segmentPath := filepath.Join(dir, "decoded-input."+wireContainer)
	if err := os.WriteFile(segmentPath, mediaBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-xerror", "-i", segmentPath, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("decode real %s: %v: %s", container, err, output)
	}
	t.Logf("surface=%s advertised=%s request=%s init=%t decoded=true", protocol, wireContainer, selected.Decision.Container, initName != "")
	sources := h.m.Sessions()
	if len(sources) != 1 || sources[0].Source == nil || sources[0].Source.ReleaseID() != "synthetic-source" || core.SourceRevision(sources[0]) != target.ETag {
		t.Fatalf("wrong opened source: %+v", sources)
	}
	read := make([]byte, len(sourceBytes))
	if _, err := sources[0].Source.ReadAt(read, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if sha256.Sum256(read) != expectedHash || counted.probes.Load() != 1 || engine.opens.Load() != 1 {
		t.Fatalf("source mismatch or repeated probe: probes=%d opens=%d", counted.probes.Load(), engine.opens.Load())
	}
	t.Logf("source-hash=%x revision=%s scope=matched probes=%d opens=%d", expectedHash, target.ETag, counted.probes.Load(), engine.opens.Load())
	grants.revoked.Store(true)
	if denied := fetch(base + "base/" + segmentName); denied.Code != http.StatusNotFound {
		t.Fatalf("revoked segment status %d", denied.Code)
	}
	if initName != "" {
		if denied := fetch(base + "base/" + initName); denied.Code != http.StatusNotFound {
			t.Fatal("revoked init", denied.Code)
		}
	}
	grants.revoked.Store(false)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, sourceBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if changed := fetch(base + "base/" + segmentName); changed.Code != http.StatusNotFound {
		t.Fatalf("changed source status %d", changed.Code)
	}
	t.Log("revocation=denied source-replacement=denied")
}
