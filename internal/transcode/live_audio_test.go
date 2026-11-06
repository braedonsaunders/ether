package transcode

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestLiveMissingAudioRetainsTrackAndRetriesSource(t *testing.T) {
	fr := newFakeRunner()
	fr.eofAfter = 2
	first := true
	fr.failStart = func(args []string) (string, bool) {
		if first {
			first = false
			return "Stream map '0:1' matches no streams.", true
		}
		return "", false
	}
	m := newTestManager(t, fr, config.Transcode{})
	info := core.MediaInfo{Container: "mpegts", Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 640, Height: 360, FrameRate: 25},
		{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000},
	}}
	s := startSession(t, m, core.HLSRequest{SessionID: "live-audio", LiveInput: true, InputURL: "http://127.0.0.1:1/live", Info: info,
		Decision: core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, VideoCodec: "h264", AudioCodec: "aac", AudioStream: 1, Container: ContainerMPEGTS}})
	rec := get(t, s, 0)
	if rec.Code != 200 || rec.Body.Len() == 0 {
		t.Fatalf("segment: %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "source audio reconnect", func() bool { return fr.startCount() >= 3 })
	fr.mu.Lock()
	starts := append([][]string(nil), fr.starts...)
	fr.mu.Unlock()
	if strings.Contains(strings.Join(starts[0], " "), "anullsrc") {
		t.Fatal("initial live input replaced unnecessarily")
	}
	silent := strings.Join(starts[1], " ")
	if !strings.Contains(silent, "anullsrc=r=48000:cl=2c") || !strings.Contains(silent, "-map 1:a:0") || !strings.Contains(silent, "-c:a aac") || strings.Contains(silent, "-c:a copy") {
		t.Fatalf("silence run: %s", silent)
	}
	normal := strings.Join(starts[2], " ")
	if strings.Contains(normal, "anullsrc") || !strings.Contains(normal, "-map 0:1") || !strings.Contains(normal, "-c:a copy") {
		t.Fatalf("returning source: %s", normal)
	}
	if s.decision.AudioCopy != true {
		t.Fatal("run fallback changed session's announced decision")
	}
}

func TestSilenceAudioArgumentsPreserveOtherInputs(t *testing.T) {
	spec := runSpec{SilenceAudio: true, LiveInput: true, Input: "http://127.0.0.1/live", Info: core.MediaInfo{Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 640, Height: 360},
		{Index: 1, Type: "audio", Codec: "ac3", Channels: 6, SampleRate: 48000, ChannelLayout: "5.1(side)"},
		{Index: 2, Type: "subtitle", Codec: "subrip", External: true, ExternalURL: "http://127.0.0.1/sub"},
	}}, Decision: core.Decision{Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1, SubtitleStream: 2}, SegDur: 4 * time.Second}
	if missingLiveAudio(spec, "Stream map '0:0' matches no streams.") {
		t.Fatal("missing video was misclassified as missing audio")
	}
	args := strings.Join(buildCodecArgs(spec), " ")
	if !strings.Contains(args, "anullsrc=r=48000:cl=5.1(side)") || !strings.Contains(args, "-map 1:0 -c:s copy") || !strings.Contains(args, "-map 2:a:0") || !strings.Contains(args, "-c:a ac3 -ac 6 -ar 48000") {
		t.Fatalf("inputs: %s", args)
	}
}

type missingAudioHTTPRunner struct{ *fakeRunner }

func (r *missingAudioHTTPRunner) Start(cmd Command) (Process, error) {
	r.mu.Lock()
	first := len(r.starts) == 0
	r.starts = append(r.starts, cmd.Args)
	r.mu.Unlock()
	p := &fakeProc{done: make(chan struct{}), kill: make(chan struct{}), resumed: make(chan struct{})}
	close(p.resumed)
	go func() {
		defer close(p.done)
		if first {
			_, _ = io.WriteString(cmd.Stderr, "Stream map '0:1' matches no streams.\n")
			p.err = errors.New("audio absent")
			return
		}
		_, _ = io.WriteString(cmd.Stdout, "\x1aE\xdf\xa3silent-audio-stream")
	}()
	return p, nil
}

func TestLiveHTTPMissingAudioRetriesBeforeHeader(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	m.runner = &missingAudioHTTPRunner{fr}
	info := httpInfo()
	info.Duration = 0
	hs, err := m.StartHLS(context.Background(), core.HLSRequest{SessionID: "live-http-audio", LiveInput: true, InputURL: "http://127.0.0.1/live", Info: info, Protocol: core.ProtocolHTTP,
		Decision: core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}})
	if err != nil {
		t.Fatal(err)
	}
	stream := hs.(core.HTTPSession)
	for range 2 {
		rec := httptest.NewRecorder()
		stream.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 || strings.Count(rec.Body.String(), "\x1aE\xdf\xa3") != 1 {
			t.Fatalf("response %d %q", rec.Code, rec.Body.String())
		}
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.starts) != 3 || !strings.Contains(strings.Join(fr.starts[1], " "), "anullsrc") || strings.Contains(strings.Join(fr.starts[2], " "), "anullsrc") {
		t.Fatalf("runs: %v", fr.starts)
	}
}
