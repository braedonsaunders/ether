package livetv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type dippingContinuousRunner struct {
	transcode.ContinuousRunner
	starts atomic.Int64
	dipped atomic.Bool
}

func (r *dippingContinuousRunner) StartEncoder(ctx context.Context, request transcode.EncoderRequest) (transcode.ContinuousEncoder, error) {
	encoder, err := r.ContinuousRunner.StartEncoder(ctx, request)
	if err != nil {
		return nil, err
	}
	r.starts.Add(1)
	return &dippingContinuousEncoder{ContinuousEncoder: encoder, origin: time.Now(), dipped: &r.dipped}, nil
}

type dippingContinuousEncoder struct {
	transcode.ContinuousEncoder
	origin time.Time
	once   sync.Once
	dipped *atomic.Bool
}

func (e *dippingContinuousEncoder) Write(ctx context.Context, frame transcode.RawFrame) error {
	if time.Since(e.origin) >= 15*time.Second {
		e.once.Do(func() {
			e.dipped.Store(true)
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
		})
	}
	return e.ContinuousEncoder.Write(ctx, frame)
}

// The broadcast clock, not a hand-fed encoder, owns this outage and recovery.
// Decode both viewer transports straight through, including the screen cut.
func TestBroadcastRealContinuity(t *testing.T) {
	if testing.Short() {
		t.Skip("real ffmpeg continuity")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg absent")
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe absent")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(dir, "outputs")}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithoutHWDetection())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	format := transcode.RawFormat{Width: 320, Height: 180, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
	origin := time.Now()
	attempts := 0
	plan := broadcastPlan{id: "outage", open: func(context.Context) (transcode.Decoder, func(), error) {
		attempts++
		return &broadcastFakeDecoder{origin: origin, format: format, closed: make(chan struct{}), stall: true}, nil, nil
	}}
	// A sleeping encoder represents a scheduling dip after viewers have joined.
	// It must not drop audio ticks, restart the rendition, or break either mux.
	runner := &dippingContinuousRunner{ContinuousRunner: manager}
	b, err := newBroadcast(ctx, runner, broadcastFakePlanner{plan: plan}, format)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	index, err := b.addRendition(ctx, "main", core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", MaxHeight: 180, BitrateKbps: 500, AudioChannels: 2, AudioSampleRate: 48000})
	if err != nil {
		t.Fatal(err)
	}
	b.start()
	// Cold first segment and an already-running output's view are timed separately.
	cold := time.Now()
	if _, err := index.Wait(ctx, -1); err != nil {
		t.Fatal(err)
	}
	t.Logf("cold first finished segment: %s", time.Since(cold))
	s := testService(t, Options{Transcoder: manager})
	channel := testChannel()
	channel.Items = []Item{{Key: 1, Title: "Long programme", DurationMS: 3600000}}
	if _, err := s.SaveChannel(ctx, channel); err != nil {
		t.Fatal(err)
	}
	user := core.User{ID: 1, IsOwner: true}
	ssctx, sscancel := context.WithCancel(ctx)
	ss := &session{ctx: ssctx, cancel: sscancel, ready: true, user: user, streamGrant: "test-grant", info: SessionInfo{ID: "continuous-test", Channel: channel, UserID: 1, MediaInfo: &core.MediaInfo{Container: "mpegts", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Width: 320, Height: 180, FrameRate: 30}, {Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}}, broadcast: &broadcastLease{broadcast: b, rendition: "main"}}
	s.mu.Lock()
	s.sessions[ss.info.ID] = ss
	s.mu.Unlock()
	httpCtx, httpCancel := context.WithCancel(ctx)
	defer httpCancel()
	httpCapture := httptest.NewRecorder()
	httpDone := make(chan error, 1)
	go func() {
		request := httptest.NewRequest("GET", "/start?protocol=http", nil).WithContext(httpCtx)
		httpDone <- s.ServeHTTP(httpCapture, request, ss.info.ID, &user)
	}()
	timeLeft := time.Until(origin.Add(40 * time.Second))
	if timeLeft > 0 {
		select {
		case <-time.After(timeLeft):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	segments := index.Since(-1, 100)
	if runner.starts.Load() != 1 || !runner.dipped.Load() {
		t.Fatal("encoder dip did not preserve the original rendition")
	}
	if len(segments) < 8 {
		t.Fatalf("only %d segments", len(segments))
	}
	for i, seg := range segments {
		expected := 4 * time.Second
		switch i {
		case 0, 1:
			expected = time.Second
		case 2:
			expected = 2 * time.Second
		}
		if seg.Seq != int64(i) || seg.Duration != expected {
			t.Fatal("non-contiguous timeline", segments)
		}
		if i > 0 && !seg.Start.Equal(segments[i-1].Start.Add(segments[i-1].Duration)) {
			t.Fatal("wall-clock gap")
		}
	}
	httpCancel()
	select {
	case err := <-httpDone:
		if err != nil {
			t.Fatal("Matroska attach", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if httpCapture.Code != 200 || httpCapture.Body.Len() == 0 || httpCapture.Header().Get("Content-Type") != "video/x-matroska" {
		t.Fatal("Matroska response", httpCapture.Code, httpCapture.Header(), httpCapture.Body.Len())
	}
	warm := time.Now()
	playlist := httptest.NewRecorder()
	s.ServePlaylist(playlist, httptest.NewRequest("GET", "/index.m3u8", nil), ss.info.ID, &user)
	t.Logf("warm playlist: %s", time.Since(warm))
	if playlist.Code != 200 || strings.Contains(playlist.Body.String(), "DISCONTINUITY") || !strings.Contains(playlist.Body.String(), "PROGRAM-DATE-TIME") {
		t.Fatal("HLS view", playlist.Code, playlist.Body.String())
	}
	mpd := httptest.NewRecorder()
	if err := s.ServeDASH(mpd, httptest.NewRequest("GET", "/start.mpd", nil), ss.info.ID, &user); err != nil {
		t.Fatal(err)
	}
	if strings.Count(mpd.Body.String(), "<Period ") != 1 || !strings.Contains(mpd.Body.String(), "test-grant") {
		t.Fatal("DASH view", mpd.Body.String())
	}
	if !strings.Contains(mpd.Body.String(), `availabilityStartTime="`+segments[0].Start.UTC().Format(time.RFC3339Nano)+`"`) {
		t.Fatal("DASH origin shifted after variable openings or DVR eviction", mpd.Body.String())
	}

	var ts, mp4 bytes.Buffer
	init := httptest.NewRecorder()
	index.ServeInit(init, httptest.NewRequest("GET", "/", nil))
	mp4.Write(init.Body.Bytes())
	for _, seg := range segments {
		rec := httptest.NewRecorder()
		index.ServeTS(rec, httptest.NewRequest("GET", "/", nil), seg.Seq)
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		ts.Write(rec.Body.Bytes())
		rec = httptest.NewRecorder()
		index.ServeFMP4(rec, httptest.NewRequest("GET", "/", nil), seg.Seq)
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		mp4.Write(rec.Body.Bytes())
	}
	for name, data := range map[string][]byte{"captured.ts": ts.Bytes(), "captured.mp4": mp4.Bytes(), "captured.mkv": httpCapture.Body.Bytes()} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "warning", "-xerror", "-i", path, "-f", "null", "-").CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) != 0 {
			t.Fatalf("%s straight decode: %v %s", name, err, out)
		}
		out, err := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "error", "-show_packets", "-show_streams", "-of", "json", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Packets []struct {
				Stream int    `json:"stream_index"`
				DTS    string `json:"dts_time"`
			} `json:"packets"`
			Streams []struct {
				Codec         string `json:"codec_name"`
				Width, Height int
			} `json:"streams"`
		}
		if err := json.Unmarshal(out, &probe); err != nil {
			t.Fatal(err)
		}
		last := map[int]float64{}
		for _, packet := range probe.Packets {
			dts, err := strconv.ParseFloat(packet.DTS, 64)
			if err != nil {
				continue
			}
			if old, ok := last[packet.Stream]; ok && dts < old {
				t.Fatal("non-monotonic DTS", name)
			}
			last[packet.Stream] = dts
		}
		if len(probe.Streams) != 2 || probe.Streams[0].Codec != "h264" || probe.Streams[0].Width != 320 || probe.Streams[0].Height != 180 || probe.Streams[1].Codec != "aac" {
			t.Fatalf("contract changed: %+v", probe.Streams)
		}
	}
	b.close()
	if attempts != 2 {
		t.Fatalf("decoder retry count %d", attempts)
	}
}
