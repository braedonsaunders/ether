package plex

import (
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// Exercise HTTP negotiation with the real media decider, but no encoder or
// player. This demonstrates request compatibility, not native-client ABR.
type qualityTranscoder struct{ *dashTranscoder }

func (t *qualityTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	t.fakeTranscoder.Decide(info, caps, opts)
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}

func TestStartRenegotiatesQualityAndTracks(t *testing.T) {
	for _, transport := range []string{"ts", "mkv", "dash", "vidaa"} {
		t.Run(transport, func(t *testing.T) {
			f, dt := dashFixture()
			defer f.srv.Close()
			f.srv.d.Transcoder = &qualityTranscoder{dt}
			protocol, ext := "hls", "m3u8"
			headers := []string{"X-Plex-Product", "Plex for Apple TV", "X-Plex-Platform", "tvOS"}
			if transport == "vidaa" {
				headers = []string{"X-Plex-Product", "Plex for VIDAA"}
			}
			if transport == "mkv" {
				headers = append(headers, "X-Plex-Client-Profile-Extra", "add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264&audioCodec=aac,eac3)")
			} else if transport == "dash" {
				protocol, ext = "dash", "mpd"
			}
			query := "path=%2Flibrary%2Fmetadata%2F1000000000000550&directPlay=0&directStream=1&session=quality&location=wan&protocol=" + protocol
			start := "/video/:/transcode/universal/start." + ext + "?"
			get := func(path string) {
				t.Helper()
				if rec := f.get(t, path, headers...); rec.Code != 200 {
					t.Fatalf("request failed: %d %s", rec.Code, rec.Body.String())
				}
			}
			request := func() core.HLSRequest {
				t.Helper()
				if protocol == "dash" {
					return dt.session("quality").req
				}
				hs, ok := f.trans.Session("quality")
				if !ok {
					t.Fatal("missing HLS session")
				}
				return hs.(*fakeHLS).req
			}
			get(start + query + "&maxVideoBitrate=12000&offset=12.5")
			first := request()
			if first.Decision.Kind != core.DecisionRemux || f.trans.lastOpts.MaxBitrateKbps != 8000 {
				t.Fatalf("initial decision/remote owner ceiling: %+v / %+v", first.Decision, f.trans.lastOpts)
			}
			get(start + query + "&maxVideoBitrate=12000&offset=12.5")
			if len(f.trans.stopped) != 0 {
				t.Fatal("identical start restarted encoder")
			}

			// No intervening /decision: a changed start at exactly the same
			// position must replace the cached high-quality decision.
			low := query + "&maxVideoBitrate=2000&videoResolution=1280x720&offset=12.5"
			get(start + low)
			second := request()
			if len(f.trans.stopped) != 1 || second.Decision.Kind != core.DecisionTranscode || second.Decision.VideoCopy || second.Decision.BitrateKbps > 2000 || second.Decision.MaxHeight > 720 || f.trans.lastOpts.MaxHeight != 720 || second.StartOffset != 12500*time.Millisecond {
				t.Fatalf("quality change ignored: stopped=%v request=%+v", f.trans.stopped, second)
			}
			if second.Decision.AudioStream != first.Decision.AudioStream || second.Decision.AudioCodec != "aac" || second.Decision.AudioCopy || second.InputURL != first.InputURL {
				t.Fatalf("quality change lost source/audio: first=%+v second=%+v", first, second)
			}
			get(start + low)
			if len(f.trans.stopped) != 1 {
				t.Fatal("repeated low quality restarted encoder")
			}

			// Track changes must also renegotiate a cached conversion.
			id := f.bindStream(fightClub.Key, 2, sampleInfo.Streams[2])
			get(start + low + fmt.Sprintf("&audioStreamID=%d", id))
			third := request()
			if len(f.trans.stopped) != 2 || third.Decision.AudioStream != 2 || third.StartOffset != second.StartOffset {
				t.Fatalf("audio change ignored: %+v", third)
			}

			// Raising quality still observes the owner's remote ceiling and
			// restores the selected source audio when no override was stored.
			get(start + query + "&maxVideoBitrate=20000&offset=37.25")
			if len(f.trans.stopped) != 3 || f.trans.lastCaps.MaxBitrateKbps != 8000 || f.trans.lastOpts.MaxBitrateKbps != 8000 || request().StartOffset != 37250*time.Millisecond {
				t.Fatalf("upshift bypassed policy or offset: %+v", request())
			}
		})
	}
}
