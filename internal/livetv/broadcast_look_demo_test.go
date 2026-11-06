package livetv

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var lookDemoDir = flag.String("look-demo", "", "record the real cached-look switcher and signed HLS")

type lookDemoPlayback struct {
	core.Playback
	path  string
	count atomic.Int64
}

func (p *lookDemoPlayback) Prepare(context.Context, *core.User, core.RatingKey, core.ClientCaps) (*core.PlaySession, error) {
	return &core.PlaySession{ID: fmt.Sprint(p.count.Add(1))}, nil
}
func (p *lookDemoPlayback) InputURL(string) string { return p.path }
func (p *lookDemoPlayback) Stop(string)            {}

type lookDemoPlanner struct {
	*scheduleBroadcastPlanner
	rows []Programme
}

func (p *lookDemoPlanner) plans(now time.Time) (broadcastPlan, *broadcastPlan) {
	var current broadcastPlan
	var next *broadcastPlan
	for _, row := range p.rows {
		plan := p.programme(row)
		if !now.Before(row.Start) && now.Before(row.End) {
			current = plan
		} else if row.Start.After(now) {
			next = &plan
			break
		}
	}
	return current, next
}

// The recording uses a deterministic Schedule excerpt rebased onto the capture
// clock. Every look master comes through the actual On Air render queue/cache;
// the reel is uploaded and converted by that service, with the real runners.
func TestBroadcastLookDemo(t *testing.T) {
	if *lookDemoDir == "" {
		t.Skip("opt-in owner recording")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip(err)
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	if err = os.MkdirAll(*lookDemoDir, 0750); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(dir, "out"), HWAccel: "none"}, logger, transcode.WithoutHWDetection())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	source := filepath.Join(dir, "programme.mp4")
	reelSource := filepath.Join(dir, "station-reel.mp4")
	for _, input := range []struct{ path, filter, duration, freq string }{{source, "testsrc2=size=1920x1080:rate=30", "20", "440"}, {reelSource, "smptebars=size=1920x1080:rate=30", "8", "660"}} {
		if out, e := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-f", "lavfi", "-i", input.filter, "-f", "lavfi", "-i", "sine=frequency="+input.freq+":sample_rate=48000", "-t", input.duration, "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "120", "-c:a", "aac", "-ac", "2", input.path).CombinedOutput(); e != nil {
			t.Fatalf("source:%v %s", e, out)
		}
	}
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	graphics, err := onair.New(onair.Options{DB: db, Dir: filepath.Join(dir, "cache"), FFmpeg: func() string { return ff }, Probe: manager.Probe, Logger: logger, StaticRequests: func(context.Context, string) []core.ClipRequest { return []core.ClipRequest{{Moment: "disabled"}} }})
	if err != nil {
		t.Fatal(err)
	}
	defer graphics.Close()
	look, err := graphics.Look(ctx, "meridian")
	if err != nil {
		t.Fatal(err)
	}
	templates, err := graphics.Templates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range templates {
		if template.ID == "clean" {
			id := look.ID
			look = template.Style
			look.ID = id
			break
		}
	}
	look.Brand.Textures = nil
	for _, moment := range []string{"ident", "break", "back"} {
		look.Roles[moment] = onair.RoleConfig{Duration: 4}
		for i := range look.Scenes {
			if look.Scenes[i].Role == moment {
				look.Scenes[i].Document.Duration = 4
				look.Scenes[i].Document.Poster = 2
			}
		}
	}
	if _, err = graphics.SaveLook(ctx, look); err != nil {
		t.Fatal(err)
	}
	reel, err := graphics.SaveReel(ctx, onair.Reel{Name: "Station reel", Order: "sequence"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(reelSource)
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := graphics.UploadClip(ctx, reel.ID, "Station reel colour bars", file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	graphics.Start()
	var reelPath string
	for reelPath == "" {
		reelPath, _ = graphics.ReelClipPath(ctx, reel.ID, uploaded.ID)
		if reelPath != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	reel, err = graphics.ReelSnapshot(ctx, reel.ID)
	if err != nil {
		t.Fatal(err)
	}
	anchor := dayStart(time.Now(), time.UTC)
	c := Channel{ID: "look-demo", Name: "MERIDIAN", Number: "7", Kind: "virtual", Enabled: true, Mode: "order", Timezone: "UTC", Epoch: anchor, Items: []Item{{Key: 1, Title: "The Evening Programme", EpisodeTitle: "A shared continuous channel", DurationMS: 16000}}, Look: &ChannelLook{LookID: look.ID, Ident: true, Breaks: "between", BreakContent: "reel", ReelID: reel.ID, BreakSeconds: 8, Placement: "insert", Overlays: "always", Tagline: "A world of cinema"}}
	s := testService(t, Options{Graphics: graphics, Playback: &lookDemoPlayback{path: source}, Transcoder: manager})
	s.reelSnapshots = map[string]*ReelSnapshot{reel.ID: {ID: reel.ID, Anchor: anchor, Order: reel.Order, Clips: []ReelClip{{ID: uploaded.ID, Name: uploaded.Name, Duration: time.Duration(reel.Clips[0].DurationMS) * time.Millisecond, Path: reelPath}}}}
	c = s.graphicsChannel(c)
	rows := Schedule(c, anchor, anchor.Add(60*time.Second))
	if len(rows) < 6 {
		t.Fatal("incomplete rundown", rows)
	}
	for i := range rows {
		if rows[i].Item.Key == 0 && rows[i].Item.ReelClipID == "" {
			request := s.clipRequest(c, rows[i])
			rows[i].graphicsRequest = &request
			graphics.Ensure(ctx, request)
		}
	}
	introRequest := s.clipRequest(c, Programme{Item: Item{Slate: "ident"}})
	graphics.Ensure(ctx, introRequest)
	for {
		ready := true
		for _, row := range rows {
			if row.graphicsRequest != nil {
				if _, ok := graphics.Clip(ctx, *row.graphicsRequest); !ok {
					ready = false
				}
			}
		}
		if _, ok := graphics.Clip(ctx, introRequest); !ok {
			ready = false
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Log("real On Air look masters and converted reel are ready")
	origin := time.Now().Add(2 * time.Second)
	for i := range rows {
		rows[i].Start = origin.Add(rows[i].Start.Sub(anchor))
		rows[i].End = origin.Add(rows[i].End.Sub(anchor))
		rows[i].ID = fmt.Sprintf("%s-%d", c.ID, rows[i].Start.UnixMilli())
	}
	format := transcode.RawFormat{Width: 1920, Height: 1080, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
	planner := &lookDemoPlanner{scheduleBroadcastPlanner: &scheduleBroadcastPlanner{s: s, channel: c, user: core.User{ID: 1, IsOwner: true}, format: format, runner: manager}, rows: rows}
	b, err := newBroadcast(ctx, manager, planner, format)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	if _, err = b.addRendition(ctx, "look", core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", MaxHeight: 1080, BitrateKbps: 6000, AudioChannels: 2, AudioSampleRate: 48000}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	user := core.User{ID: 1, IsOwner: true}
	ssctx, sscancel := context.WithCancel(ctx)
	ss := &session{ctx: ssctx, cancel: sscancel, ready: true, user: user, last: time.Now(), streamGrant: "look-demo-grant", info: SessionInfo{ID: "look-demo", Channel: c, UserID: 1}, broadcast: &broadcastLease{broadcast: b, rendition: "look", height: 1080}}
	s.mu.Lock()
	s.sessions[ss.info.ID] = ss
	s.mu.Unlock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "index.m3u8") {
			s.ServePlaylist(w, r, ss.info.ID, &user)
			return
		}
		n, e := strconv.Atoi(strings.TrimSuffix(filepath.Base(r.URL.Path), ".ts"))
		if e != nil {
			http.NotFound(w, r)
			return
		}
		s.ServeRelaySegment(w, r, ss.info.ID, n, &user)
	}))
	defer server.Close()
	// Reset only the harness clock after all slow DB/process setup, then rebase
	// the immutable excerpt together. Production starts immediately after acquire.
	actual := time.Now()
	delta := actual.Sub(origin)
	for i := range planner.rows {
		planner.rows[i].Start = planner.rows[i].Start.Add(delta)
		planner.rows[i].End = planner.rows[i].End.Add(delta)
	}
	b.origin = actual
	b.introUntil = actual.Add(4 * time.Second)
	planner.joinAt = b.introUntil
	b.start()
	capture := filepath.Join(dir, "captured.ts")
	captured, err := os.Create(capture)
	if err != nil {
		t.Fatal(err)
	}
	defer captured.Close()
	next := int64(0)
	for next < 15 {
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/livetv/sessions/look-demo/0/index.m3u8", nil)
		res, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("playlist:%d %s", res.StatusCode, body)
		}
		if strings.Contains(string(body), "DISCONTINUITY") {
			t.Fatal("discontinuity")
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			path, _, _ := strings.Cut(line, "?")
			seq, e := strconv.ParseInt(strings.TrimSuffix(filepath.Base(path), ".ts"), 10, 64)
			if e != nil {
				t.Fatal(e)
			}
			if seq < next {
				continue
			}
			if seq != next {
				t.Fatal("lost segment", seq, next)
			}
			ref, _ := url.Parse(line)
			mediaReq, _ := http.NewRequestWithContext(ctx, "GET", req.URL.ResolveReference(ref).String(), nil)
			media, e := server.Client().Do(mediaReq)
			if e != nil {
				t.Fatal(e)
			}
			if media.StatusCode != 200 {
				media.Body.Close()
				t.Fatal("segment", media.StatusCode)
			}
			_, e = io.Copy(captured, media.Body)
			media.Body.Close()
			if e != nil {
				t.Fatal(e)
			}
			next++
			if next == 15 {
				break
			}
		}
		if next < 15 {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	captured.Close()
	b.close()
	output := filepath.Join(*lookDemoDir, "look-demo.mp4")
	if out, e := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-i", capture, "-c", "copy", "-movflags", "+faststart", output).CombinedOutput(); e != nil {
		t.Fatalf("remux:%v %s", e, out)
	}
	if out, e := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-xerror", "-i", output, "-f", "null", "-").CombinedOutput(); e != nil {
		t.Fatalf("decode:%v %s", e, out)
	}
	raw, err := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "error", "-show_packets", "-show_format", "-show_entries", "packet=stream_index,pts_time,dts_time:format=duration,bit_rate", "-of", "json", output).Output()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Packets []struct {
			Stream int    `json:"stream_index"`
			PTS    string `json:"pts_time"`
			DTS    string `json:"dts_time"`
		}
		Format struct {
			Duration string
			Bitrate  string `json:"bit_rate"`
		}
	}
	if err = json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	lastPTS, lastDTS := map[int]float64{}, map[int]float64{}
	for _, packet := range probe.Packets {
		pts, e := strconv.ParseFloat(packet.PTS, 64)
		if e != nil {
			t.Fatal(e)
		}
		dts, e := strconv.ParseFloat(packet.DTS, 64)
		if e != nil {
			t.Fatal(e)
		}
		if old, ok := lastPTS[packet.Stream]; ok && pts < old {
			t.Fatal("non-monotonic PTS")
		}
		if old, ok := lastDTS[packet.Stream]; ok && dts < old {
			t.Fatal("non-monotonic DTS")
		}
		lastPTS[packet.Stream] = pts
		lastDTS[packet.Stream] = dts
	}
	duration, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	bitrate, _ := strconv.ParseFloat(probe.Format.Bitrate, 64)
	metadata := map[string]any{"caption": "MERIDIAN: ident → programme with bug and lower third → break → converted station reel → back → scheduled ident → programme. Real On Air service, local decoders, one switcher/encoder, signed shared HLS, copy-remux.", "resolution": "1920x1080", "bitrate_kbps": bitrate / 1000, "duration_s": duration, "discontinuity_tags": 0, "pts_monotonic": true, "dts_monotonic": true, "timeline_seconds": map[string]string{"0-4": "ident", "4-16": "programme with bug and lowerthird", "16-20": "break", "20-28": "converted reel", "28-32": "back", "32-36": "scheduled ident", "36-52": "programme with bug and lowerthird"}}
	data, _ := json.MarshalIndent(metadata, "", "  ")
	if err = os.WriteFile(filepath.Join(*lookDemoDir, "look-demo.json"), append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	t.Log(string(data))
}
