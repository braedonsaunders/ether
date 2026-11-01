package sessions

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

// TestSwiftfinBigBuckBunnyDirectPlaysBothAudio is the lab file Swiftfin
// played: 1080p h264 plus MP3 stereo and AC3 5.1. The device profile allows
// both audio codecs, so the plan is direct play of the file. Both streams
// stay in the media the client lists. A single AAC 5.1 menu row is a
// different release, not a rewritten transcode of this one.
func TestSwiftfinBigBuckBunnyDirectPlaysBothAudio(t *testing.T) {
	var body struct {
		DeviceProfile caps.DeviceProfile `json:"DeviceProfile"`
	}
	if err := json.Unmarshal(swiftfinPlaybackInfo, &body); err != nil {
		t.Fatal(err)
	}
	client, err := (caps.Builder{}).Build(context.Background(), caps.DeviceInfo{Product: "Swiftfin tvOS", Platform: "tvOS", Device: "Apple TV"}, body.DeviceProfile, nil, "Swiftfin/1.4.1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(client.AudioCodecs, "mp3") || !slices.Contains(client.AudioCodecs, "ac3") {
		t.Fatalf("profile audio = %v", client.AudioCodecs)
	}
	broker := &Broker{deps: BrokerDeps{Transcoder: profileTranscoder{}}}
	no := false
	info := core.MediaInfo{Container: "mp4", BitrateKbps: 7980, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Profile: "High", Level: 42, BitDepth: 8, Width: 1920, Height: 1080, PixelFormat: "yuv420p", DolbyVisionConfigurationPresent: &no, HDR10Plus: &no, Default: true},
		{Index: 1, Type: "audio", Codec: "mp3", Channels: 2, ChannelLayout: "stereo", SampleRate: 48000, Default: true, Title: "GPAC ISO Audio Handler"},
		{Index: 2, Type: "audio", Codec: "ac3", Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, Default: true, Title: "GPAC ISO Audio Handler"},
	}}
	req := frontend.PlanRequest{Caps: client, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}}
	d, err := broker.decideProfile(&core.PlaySession{Info: info}, req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != core.DecisionDirectPlay || !d.AudioCopy || d.AudioCodec == "aac" {
		t.Fatalf("want direct play of the source audio, got %+v", d)
	}
	if len(info.Streams) != 3 {
		t.Fatalf("source streams changed: %+v", info.Streams)
	}
}
