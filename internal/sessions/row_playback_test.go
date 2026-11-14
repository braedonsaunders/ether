package sessions

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

// Captured bundled jellyfin-web Chrome profile. Output predicates must see
// what the encoder emits; source predicates must still see the original file.
func TestJellyfinWebRowPlaybackProfile(t *testing.T) {
	data, err := os.ReadFile("testdata/jellyfin-web-rowplay.json")
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ DeviceProfile caps.DeviceProfile }
	if err = json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	client, err := (caps.Builder{}).Build(context.Background(), caps.DeviceInfo{Protocol: "jellyfin", Product: "Jellyfin Web"}, body.DeviceProfile, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	info := core.MediaInfo{Container: "mkv", BitrateKbps: 26000, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Level: 153, Width: 3840, Height: 2160, FrameRate: 23.976, BitDepth: 10, ColorTransfer: "smpte2084", DolbyVision: true, DolbyVisionProfile: 8, DolbyVisionCompatibilityID: 1, DolbyVisionConfigurationPresent: &yes, ProbedInterlaced: &no},
		{Index: 1, Type: "audio", Codec: "eac3", Channels: 6, SampleRate: 48000, Language: "rus", Default: true},
		{Index: 2, Type: "audio", Codec: "eac3", Channels: 6, SampleRate: 48000, Language: "eng"},
	}}
	b := &Broker{deps: BrokerDeps{Transcoder: profileTranscoder{}}}
	for _, audio := range []int{1, 2} {
		for _, copyVideo := range []bool{true, false} {
			req := frontend.PlanRequest{Caps: client, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: copyVideo, AudioStream: audio, SubtitleStream: -1}}}
			decision, err := b.decideProfile(&core.PlaySession{Info: info}, req)
			if err != nil {
				t.Fatalf("audio=%d copy=%v: %v", audio, copyVideo, err)
			}
			if decision.AudioStream != audio || decision.AudioCopy || decision.AudioCodec != "aac" {
				t.Fatalf("wrong audio: %+v", decision)
			}
			if copyVideo {
				if !decision.VideoCopy || !decision.StripDolbyVision || decision.VideoCodec != "hevc" {
					t.Fatalf("HDR10 base not preserved: %+v", decision)
				}
			} else if decision.VideoCopy || decision.VideoCodec != "h264" {
				t.Fatalf("missing H264 fallback: %+v", decision)
			}
			if _, ok := b.hlsOutputDecision(&core.PlaySession{Info: info}, req.Caps, req.Intent.Options, nil, decision); !ok {
				t.Fatalf("planned output cannot start: %+v", decision)
			}
			if profileDirectAllowed(client.Profiles, info, decision) {
				t.Fatal("source predicates were widened")
			}
		}
	}
	if len(info.Streams) != 3 || info.Streams[0].Codec != "hevc" || !info.Streams[0].DolbyVision || info.Streams[2].Codec != "eac3" {
		t.Fatal("output validation mutated source")
	}
	// A direct-file AAC 5.1 track is playable, while the captured streaming
	// profile permits only stereo. Progressive subtitles require that same
	// HLS demuxer even when source health would otherwise allow direct play.
	t.Run("progressive surround delivery", func(t *testing.T) {
		body.DeviceProfile.SubtitleProfiles = append(body.DeviceProfile.SubtitleProfiles, caps.SubtitleProfile{Format: progressiveSubtitleFormat, Method: "External"})
		progressive, err := (caps.Builder{}).Build(context.Background(), caps.DeviceInfo{Protocol: "jellyfin", Product: "Jellyfin Web"}, body.DeviceProfile, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		media := core.MediaInfo{Container: "mkv", Size: 4476577139, Duration: 5609675 * time.Millisecond, BitrateKbps: 6384, Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Level: 153, Width: 3840, Height: 2160, FrameRate: 23.976, BitDepth: 10, ColorTransfer: "bt709", DolbyVisionConfigurationPresent: &no, ProbedInterlaced: &no},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 6, SampleRate: 48000, Language: "eng", Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "eng"},
			{Index: 3, Type: "subtitle", Codec: "subrip", Language: "eng", Title: "English SDH"},
		}}
		for _, health := range []bool{false, true} {
			for _, selected := range []int{-1, 2, 3} {
				source := &core.PlaySession{Info: media, Resolution: &core.Resolution{HealthVerified: health, Release: core.Release{ID: "model-citizen", Protocol: core.ProtocolUsenet}}}
				req := frontend.PlanRequest{Protocol: "jellyfin", Caps: progressive, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: 1, SubtitleStream: selected}}}
				// Reproduce the rejected proposal: disabling direct play alone
				// still copies 5.1 and cannot satisfy either stereo HLS profile.
				rawOpts := req.Intent.Options
				rawOpts.DirectPlay, rawOpts.SubtitleStream = false, -1
				raw := b.deps.Transcoder.Decide(media, progressive, rawOpts)
				if !raw.AudioCopy || profileDeliveryAllowed(progressive.Profiles, raw, media, "hls") {
					t.Fatalf("capture no longer reproduces rejected surround copy: %+v", raw)
				}
				decision, err := b.decideProfile(source, req)
				if err != nil {
					t.Fatalf("health=%v subtitle=%d: %v", health, selected, err)
				}
				if decision.Kind != core.DecisionRemux || !decision.VideoCopy || decision.VideoCodec != "hevc" || decision.AudioCopy || decision.AudioCodec != "aac" || decision.BurnSubtitles || encodedAudioChannels(decision, media) != 2 {
					t.Fatalf("expected HEVC copy and stereo AAC with external text: %+v", decision)
				}
				if _, ok := b.hlsOutputDecision(source, progressive, req.Intent.Options, nil, decision); !ok {
					t.Fatalf("planned output cannot start: %+v", decision)
				}
				denied := false
				req.Intent.AllowTranscoding = &denied
				if _, err := b.decideProfile(source, req); err == nil {
					t.Fatal("ignored explicit audio conversion prohibition")
				}
			}
		}
		// The streaming limit must not narrow native direct-file decoding.
		if !profileDirectAllowed(progressive.Profiles, media, core.Decision{AudioStream: 1}) || len(progressive.AudioLimits) != 0 {
			t.Fatal("streaming channel limit leaked into source capabilities")
		}
	})

}
