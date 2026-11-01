package transcode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func httpInfo() core.MediaInfo {
	return core.MediaInfo{
		Duration: time.Minute,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
		},
	}
}

func TestHTTPSessionServesMatroska(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	hs, err := m.StartHLS(context.Background(), core.HLSRequest{
		SessionID:   "http-1",
		InputURL:    "http://127.0.0.1/src",
		Info:        httpInfo(),
		Decision:    core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mkv"},
		Protocol:    core.ProtocolHTTP,
		StartOffset: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, ok := hs.(core.HTTPSession)
	if !ok {
		t.Fatalf("StartHLS returned %T, want HTTPSession", hs)
	}
	if got, found := m.Session("http-1"); !found || got.ID() != "http-1" {
		t.Fatal("Session lookup missed HTTP session")
	}

	rec := httptest.NewRecorder()
	stream.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("ct %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Accept-Ranges") != "none" {
		t.Fatalf("accept-ranges %q", rec.Header().Get("Accept-Ranges"))
	}
	if !strings.Contains(rec.Body.String(), "fake-matroska") {
		t.Fatalf("body %q", rec.Body.String())
	}

	args := fr.lastStart()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-f matroska") || !strings.Contains(joined, "pipe:1") {
		t.Fatalf("args %q missing matroska pipe", joined)
	}
	if !strings.Contains(joined, "-ss") || !strings.Contains(joined, "-copyts") {
		t.Fatalf("args %q missing VOD seek", joined)
	}
	if strings.Contains(joined, "-c:a copy") || strings.Contains(joined, "-c copy") {
		t.Fatalf("args %q copied AAC into piped Matroska", joined)
	}

	starts := fr.startCount()
	head := httptest.NewRecorder()
	stream.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("head: %d len=%d", head.Code, head.Body.Len())
	}
	if fr.startCount() != starts {
		t.Fatal("HEAD started ffmpeg")
	}

	m.Stop("http-1")
	if _, found := m.Session("http-1"); found {
		t.Fatal("Stop left HTTP session")
	}
	closed := httptest.NewRecorder()
	stream.ServeHTTP(closed, httptest.NewRequest(http.MethodGet, "/", nil))
	if closed.Code != http.StatusGone {
		t.Fatalf("closed session: %d", closed.Code)
	}
}

func TestHTTPSessionRespectsMaxTranscodes(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{MaxTranscodes: 1})
	enc := core.Decision{Kind: core.DecisionTranscode, VideoCopy: false, AudioCopy: false, Container: "mkv", VideoCodec: "h264", AudioCodec: "aac"}
	if _, err := m.StartHLS(context.Background(), core.HLSRequest{
		SessionID: "http-cap-1", InputURL: "http://127.0.0.1/src", Info: httpInfo(), Decision: enc, Protocol: core.ProtocolHTTP,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartHLS(context.Background(), core.HLSRequest{
		SessionID: "http-cap-2", InputURL: "http://127.0.0.1/src", Info: httpInfo(), Decision: enc, Protocol: core.ProtocolHTTP,
	}); !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("second HTTP encode: %v", err)
	}
}

func TestHTTPSessionIdleDuringWrite(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{}, 1)
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{}, WithRunner(&holdHTTPRunner{fakeRunner: fr, hold: hold, started: started}), WithIdleTimeout(time.Millisecond))
	hs, err := m.StartHLS(context.Background(), core.HLSRequest{
		SessionID: "http-idle", InputURL: "http://127.0.0.1/src", Info: httpInfo(),
		Decision: core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mkv"},
		Protocol: core.ProtocolHTTP,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := hs.(core.HTTPSession)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		stream.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("http write did not start")
	}
	m.reapIdle(time.Now().Add(time.Hour))
	if _, found := m.Session("http-idle"); !found {
		t.Fatal("reaped HTTP stream while ffmpeg was writing")
	}
	close(hold)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not finish")
	}
}

type holdHTTPRunner struct {
	*fakeRunner
	hold    chan struct{}
	started chan struct{}
}

func (r *holdHTTPRunner) Start(cmd Command) (Process, error) {
	if argValue(cmd.Args, "-f") != "matroska" {
		return r.fakeRunner.Start(cmd)
	}
	r.mu.Lock()
	r.starts = append(r.starts, cmd.Args)
	r.mu.Unlock()
	p := &fakeProc{done: make(chan struct{}), kill: make(chan struct{}), resumed: make(chan struct{})}
	close(p.resumed)
	go func() {
		defer close(p.done)
		if cmd.Stdout != nil {
			_, _ = io.WriteString(cmd.Stdout, "chunk")
		}
		select {
		case r.started <- struct{}{}:
		default:
		}
		select {
		case <-r.hold:
		case <-p.kill:
			p.err = errors.New("signal: killed")
		}
	}()
	return p, nil
}
