package transcode

import (
	"math"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestVelaPlaybackClockAndBudget(t *testing.T) {
	t.Run("tone map budget preserves direct and remux", func(t *testing.T) {
		options := allow()
		options.ToneMapMaxHeight = 720
		options.ToneMapMaxBitrateKbps = 4000
		info := mustParse(t, "mkv_dv_p8.json")
		caps := plexWeb
		caps.MaxHeight = 0
		d := Decide(info, caps, options, Policy{})
		if !d.ToneMap || d.MaxHeight > 720 || d.BitrateKbps > 4000 || !strings.Contains(d.Reason, "processing budget") {
			t.Fatalf("unbounded tone map: %+v", d)
		}
		mp4 := mustParse(t, "mp4_h264_aac.json")
		if d := Decide(mp4, appleTV, options, Policy{}); d.Kind != core.DecisionDirectPlay {
			t.Fatal("processing budget constrained compatible direct play", d)
		}
		remux := mustParse(t, "mkv_truehd_atmos.json")
		if d := Decide(remux, appleTV, options, Policy{HEVCEncoder: true}); !d.VideoCopy || d.ToneMap {
			t.Fatal("processing budget constrained copy-only picture", d)
		}
		options.ToneMapMaxHeight = 360
		options.ToneMapMaxBitrateKbps = 1500
		d = Decide(info, caps, options, Policy{})
		if d.MaxHeight > 360 || d.BitrateKbps > 1500 {
			t.Fatal("explicit lower budget was raised", d)
		}
	})
	t.Run("remaining playlist and absolute fragment clock", func(t *testing.T) {
		e := newRealEnv(t, WithoutHWDetection())
		e.gen(t, "seek-origin.mp4", 12, "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-g", "24", "-c:a", "aac", "-ac", "2")
		info, err := e.m.Probe(t.Context(), e.url("seek-origin.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		origin := 6 * time.Second
		hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "offset-clock", InputURL: e.url("seek-origin.mp4"), Info: *info, StartOffset: origin, Continuous: true, Decision: core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", AudioStream: 1, SubtitleStream: -1, MaxHeight: 180, BitrateKbps: 1000, Container: ContainerFMP4}})
		if err != nil {
			t.Fatal(err)
		}
		session := hs.(*Session)
		w := httptest.NewRecorder()
		session.ServeRungPlaylist(w, httptest.NewRequest("GET", "/base/index.m3u8", nil), "base")
		playlist := w.Body.String()
		if !strings.Contains(playlist, "#EXT-X-ENDLIST") || !strings.Contains(playlist, "#EXT-X-MAP") {
			t.Fatal("not finite fMP4", playlist)
		}
		var span float64
		for _, line := range strings.Split(playlist, "\n") {
			if strings.HasPrefix(line, "#EXTINF:") {
				n, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
				if err != nil {
					t.Fatal(err)
				}
				span += n
			}
		}
		if math.Abs(span-(info.Duration-origin).Seconds()) > .001 {
			t.Fatalf("playlist span%f, want%f", span, (info.Duration - origin).Seconds())
		}
		init := get(t, session, InitSegment)
		segment := get(t, session, 0)
		if init.Code != 200 || segment.Code != 200 {
			t.Fatal("offset output unavailable", init.Code, segment.Code)
		}
		tracks, err := parseInitTracks(init.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		var videoScale uint32
		for _, track := range tracks {
			if track.Handler == "vide" {
				videoScale = track.Timescale
			}
		}
		video, _ := videoTfdt(t, segment.Body.Bytes())
		if videoScale == 0 {
			t.Fatal("missing actual video clock")
		}
		actual := float64(video) / float64(videoScale)
		if actual < origin.Seconds()-.5 || actual > origin.Seconds()+.5 {
			t.Fatalf("fragment clock%f is not absolute origin%f", actual, origin.Seconds())
		}
	})
}
