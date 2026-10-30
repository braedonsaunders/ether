package transcode

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Real FFmpeg proves that expediting a media downswitch still uses the shared
// timestamps and preserves audio. It does not simulate a browser or a link.
// The copy-primary case keeps ABRLadderUnderCopy explicit: UpdateConfig
// replaces the whole transcode config.
func TestIntegrationAdaptiveRapidMediaHandoff(t *testing.T) {
	testIntegrationAdaptiveRapidMediaHandoff(t, true)
}

// Additional encode-primary handoff. Copy and encode alignment are different
// behaviors; this does not replace the copy-primary case.
func TestIntegrationAdaptiveRapidEncodeHandoff(t *testing.T) {
	testIntegrationAdaptiveRapidMediaHandoff(t, false)
}

func testIntegrationAdaptiveRapidMediaHandoff(t *testing.T, copyPrimary bool) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(24*time.Second, 8*time.Second, 8*time.Second), withRungDwell(8*time.Second))
	e.gen(t, "adaptive.mkv", 16, "-vf", "scale=854:480", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-g", "48", "-sc_threshold", "0", "-c:a", "aac", "-ac", "2")
	info, err := e.m.Probe(t.Context(), e.url("adaptive.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	info.Keyframes = e.keyframeIndex(t, "adaptive.mkv")
	e.m.UpdateConfig(config.Transcode{ABRLadder: true, ABRLadderUnderCopy: true})
	for _, transport := range []string{ContainerMPEGTS, ContainerFMP4, core.ProtocolDASH} {
		t.Run(transport, func(t *testing.T) {
			proto, container := core.ProtocolHLS, transport
			if transport == core.ProtocolDASH {
				proto, container = core.ProtocolDASH, ContainerFMP4
			}
			d := core.Decision{Kind: core.DecisionRemux, Container: container, VideoCopy: true, AudioCopy: true, AudioStream: 1}
			if !copyPrimary {
				d = core.Decision{Kind: core.DecisionTranscode, Container: container, VideoCodec: "h264", MaxHeight: 720, BitrateKbps: 8000, AudioStream: 1, AudioCodec: "aac"}
			}
			hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "adaptive-" + transport, InputURL: e.url("adaptive.mkv"), Info: *info, Decision: d, Protocol: proto, SegmentDuration: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer hs.Close()
			s := hs.(*Session)
			if s.decision.VideoCopy != copyPrimary {
				t.Fatalf("video copy = %v, want %v", s.decision.VideoCopy, copyPrimary)
			}
			if len(s.rungs) < 2 {
				t.Fatalf("no lower rendition: %+v", s.rungs)
			}
			var previousAudio float64
			for n := 0; n < 2; n++ {
				if proto == core.ProtocolDASH {
					_ = dashGet(t, s, 0, n)
					_, previousAudio = e.probeDASH(t, "previous-audio.mp4", dashGet(t, s, 1, n))
				} else {
					_ = fetch(t, s, n)
				}
			}
			s.mu.Lock()
			s.wantTrack = s.rungTrack(s.rungs[len(s.rungs)-1].id)
			left := s.rungDwellLocked(true, 2)
			s.mu.Unlock()
			if left != 0 {
				t.Fatalf("real delivered media still held lower rung %v", left)
			}
			started := time.Now()
			var body, init []byte
			var audioFirst float64
			if proto == core.ProtocolDASH {
				body = dashGet(t, s, s.wantTrack, 2)
				_, audioFirst = e.probeDASH(t, "next-audio.mp4", dashGet(t, s, 1, 2))
				if math.Abs(previousAudio-2) > 0.1 || math.Abs(audioFirst-4) > 0.1 {
					t.Fatalf("audio timeline %v -> %v", previousAudio, audioFirst)
				}
			} else {
				rec := httptest.NewRecorder()
				s.ServeRungSegment(rec, httptest.NewRequest(http.MethodGet, "/", nil), s.rungs[len(s.rungs)-1].id, 2)
				if rec.Code != http.StatusOK {
					t.Fatalf("switch %d %s", rec.Code, rec.Body.String())
				}
				body = rec.Body.Bytes()
				if container == ContainerFMP4 {
					rec = httptest.NewRecorder()
					s.ServeRungSegment(rec, httptest.NewRequest(http.MethodGet, "/", nil), s.rungs[len(s.rungs)-1].id, InitSegment)
					if rec.Code != 200 {
						t.Fatal("missing switched init")
					}
					init = rec.Body.Bytes()
				}
			}
			elapsed := time.Since(started)
			first, end, key := e.videoSpanKey(t, container, init, body, "switched-"+transport)
			if math.Abs(first-4) > 0.1 || math.Abs(end-6) > 0.1 || !key {
				t.Fatalf("switched span %.3f..%.3f key=%v", first, end, key)
			}
			ps := e.probeFile(t, e.writeTemp(t, "audio-check-"+transport+"."+segmentExt(container), append(append([]byte(nil), init...), body...)))
			if proto != core.ProtocolDASH {
				audioFirst = startTime(t, ps, "audio")
				if container == ContainerMPEGTS {
					audioFirst -= tsMuxDelay.Seconds()
				}
				if math.Abs(audioFirst-4) > 0.1 {
					t.Fatalf("audio begins %.3f, want4", audioFirst)
				}
			}
			s.mu.Lock()
			runs := s.runSeq
			s.mu.Unlock()
			if runs != 2 {
				t.Fatalf("handoff used %d runs, want2", runs)
			}
			t.Logf("real FFmpeg: lower segment ready in %v, added dwell0, video %.3f..%.3f key=%v audio %.3f runs=%d", elapsed, first, end, key, audioFirst, runs)
		})
	}
}
