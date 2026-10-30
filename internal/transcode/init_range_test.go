package transcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Signals that serveInit reached its cancellable wait, without racing a
// sleep against the encoder or relying on a fast filesystem.
type initWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *initWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestInitRangeWaitsForActiveEncoder(t *testing.T) {
	for _, format := range []string{ContainerMatroska, ContainerFMP4, "dash-video", "dash-audio"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(format+"/"+method, func(t *testing.T) {
				s := &Session{
					m:         &Manager{opts: options{segmentTimeout: time.Second}},
					dir:       t.TempDir(),
					container: format,
					sessionEncoding: sessionEncoding{
						cur: &run{track: 0},
					},
					sessionDelivery: sessionDelivery{
						done:    []map[int]bool{{}, {}},
						changed: make(chan struct{}),
					},
					sessionAdaptation: sessionAdaptation{
						wantTrack: 0,
					},
				}
				track := 0
				if format == "dash-video" || format == "dash-audio" {
					s.proto = core.ProtocolDASH
					s.container = ContainerFMP4
					s.dash = &dashState{inits: make([]string, 2), reps: []dashRep{{Kind: "video"}, {Kind: "audio"}}}
					if format == "dash-audio" {
						track = 1
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				waitCtx := &initWaitContext{Context: ctx, entered: make(chan struct{})}
				req := httptest.NewRequest(method, "/header", nil).WithContext(waitCtx)
				req.Header.Set("Range", "bytes=0-")
				rec := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { defer close(done); s.serveInit(rec, req, track) }()
				defer func() { cancel(); <-done }()
				select {
				case <-done:
					t.Fatalf("cold init returned before publication: HTTP %d %q", rec.Code, rec.Body.String())
				case <-waitCtx.entered:
				case <-time.After(time.Second):
					t.Fatal("init did not wait")
				}
				body := []byte("encoder initialization bytes")
				if err := os.WriteFile(filepath.Join(s.dir, "ready.init"), body, 0o600); err != nil {
					t.Fatal(err)
				}
				s.mu.Lock()
				if s.isDASH() {
					s.dash.inits[track] = "ready.init"
				} else {
					s.initFile = "ready.init"
				}
				s.broadcastLocked()
				s.mu.Unlock()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("init waiter did not complete")
				}
				expected := http.StatusPartialContent
				if rec.Code != expected {
					t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
				}
				if method == http.MethodGet && rec.Body.String() != string(body) {
					t.Fatalf("body=%q", rec.Body.String())
				}
				if method == http.MethodHead && rec.Body.Len() != 0 {
					t.Fatal("HEAD returned a body")
				}
				if s.wantTrack != 0 || s.runSeq != 0 || s.deliveredCount != 0 {
					t.Fatal("init probe changed encoder selection or delivery evidence")
				}
			})
		}
	}
}

// Native HLS players (mpv/ffmpeg) always open the EXT-X-MAP init with
// Range: bytes=0-. That ranged GET on a lazy rung's init must select and
// start the rung like an unranged GET, or the rung never starts: the
// player never asks for its media segments because the init failed.
func TestHLSInitRangeStartsInactiveRung(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true})
	info := core.MediaInfo{
		Container: "mkv", Duration: 10 * time.Minute, BitrateKbps: 4000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 536, FrameRate: 23.976},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000, BitrateKbps: 192},
		},
	}
	s := startSession(t, m, core.HLSRequest{
		SessionID: "hls-rung-init-range",
		InputURL:  "http://127.0.0.1:1/source/a",
		Info:      info,
		Decision: core.Decision{
			Kind: core.DecisionTranscode, Container: ContainerFMP4,
			VideoCodec: "h264", MaxHeight: 720, BitrateKbps: 4000,
			AudioStream: 1, AudioCopy: true,
		},
	})
	var ids []string
	for _, r := range s.Rungs() {
		ids = append(ids, r.ID)
	}
	if got, want := strings.Join(ids, ","), "base,480p1500k,360p800k"; got != want {
		t.Fatalf("rungs = %s, want %s", got, want)
	}
	if fr.startCount() != 1 {
		t.Fatalf("starts = %d, want 1 (base only)", fr.startCount())
	}
	req := httptest.NewRequest(http.MethodGet, "/480p1500k/init.mp4", nil)
	req.Header.Set("Range", "bytes=0-")
	rec := httptest.NewRecorder()
	s.ServeRungSegment(rec, req, "480p1500k", InitSegment)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("ranged init of inactive rung: status=%d body=%q, want 206", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("ranged init served no bytes")
	}
	if fr.startCount() != 2 {
		t.Fatalf("starts = %d, want 2 (rung started by its ranged init)", fr.startCount())
	}
	mustContain(t, fr.lastStart(), "scale_vt=w=1146:h=480")
}

// The ranged-probe guard still holds where it was designed for: DASH.
// A ranged init GET for an inactive DASH video representation must not
// select or start it (DASH has its own videoRequested/audioTrackLocked
// semantics); the lazy HLS path is covered by
// TestHLSInitRangeStartsInactiveRung.
func TestInitRangeDoesNotStartInactiveRendition(t *testing.T) {
	s := &Session{
		m:         &Manager{opts: options{segmentTimeout: time.Second}},
		proto:     core.ProtocolDASH,
		container: ContainerFMP4,
		sessionEncoding: sessionEncoding{
			cur: &run{track: 0},
		},
		sessionDelivery: sessionDelivery{
			done:    []map[int]bool{{}, {}, {}},
			changed: make(chan struct{}),
			dash: &dashState{
				inits: make([]string, 3),
				reps:  []dashRep{{Kind: "video"}, {Kind: "audio"}, {Kind: "video"}},
			},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/header", nil)
	req.Header.Set("Range", "bytes=0-")
	rec := httptest.NewRecorder()
	s.serveInit(rec, req, 2)
	if rec.Code != http.StatusServiceUnavailable || s.wantTrack != 0 {
		t.Fatalf("status=%d selected=%d", rec.Code, s.wantTrack)
	}
}
