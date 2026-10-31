package sessions

import (
	"context"
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

//go:embed testdata/swiftfin-tvos-1.4.1-playbackinfo.json
var swiftfinPlaybackInfo []byte

// Swiftfin tvOS 1.4.1 requires VideoRangeType on h264, hevc, av1 and vp9.
// A matching range direct-plays. A miss transcodes. It never fails the plan.
func TestSwiftfinVideoRangeDirectPlaysOrTranscodes(t *testing.T) {
	var body struct {
		DeviceProfile caps.DeviceProfile `json:"DeviceProfile"`
	}
	if err := json.Unmarshal(swiftfinPlaybackInfo, &body); err != nil {
		t.Fatal(err)
	}
	client, err := (caps.Builder{}).Build(context.Background(), caps.DeviceInfo{Product: "Swiftfin", Platform: "tvOS", Device: "Apple TV"}, body.DeviceProfile, nil, "Swiftfin/1.4.1")
	if err != nil {
		t.Fatal(err)
	}
	broker := &Broker{deps: BrokerDeps{Transcoder: profileTranscoder{}}}
	req := frontend.PlanRequest{Caps: client, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: 1, SubtitleStream: -1}}}
	no := false
	play := func(video core.StreamInfo) (core.Decision, error) {
		t.Helper()
		video.Index = 0
		video.Type = "video"
		if video.Width == 0 {
			video.Width, video.Height, video.FrameRate = 1280, 544, 24
		}
		info := core.MediaInfo{Container: "mp4", BitrateKbps: 2500, Streams: []core.StreamInfo{video, {Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}
		return broker.decideProfile(&core.PlaySession{Info: info}, req)
	}
	sdr := core.StreamInfo{Codec: "h264", Profile: "High", Level: 41, BitDepth: 8, PixelFormat: "yuv420p", DolbyVisionConfigurationPresent: &no, HDR10Plus: &no}
	if d, err := play(sdr); err != nil || d.Kind != core.DecisionDirectPlay {
		t.Fatalf("8-bit untagged H.264: %+v %v", d, err)
	}
	hevcSDR := sdr
	hevcSDR.Codec = "hevc"
	hevcSDR.Profile = "Main"
	if d, err := play(hevcSDR); err != nil || d.Kind != core.DecisionDirectPlay {
		t.Fatalf("8-bit untagged HEVC: %+v %v", d, err)
	}
	hdr := core.StreamInfo{Codec: "hevc", Profile: "Main 10", Level: 150, BitDepth: 10, PixelFormat: "yuv420p10le", ColorTransfer: "smpte2084", Width: 1920, Height: 1080, FrameRate: 24, DolbyVisionConfigurationPresent: &no, HDR10Plus: &no}
	if d, err := play(hdr); err != nil || d.Kind != core.DecisionDirectPlay {
		t.Fatalf("HDR10 HEVC in the Swiftfin list: %+v %v", d, err)
	}
	dovi := hdr
	yes := true
	dovi.DolbyVision = true
	dovi.DolbyVisionProfile = 8
	dovi.DolbyVisionCompatibilityID = 1
	dovi.DolbyVisionConfigurationPresent = &yes
	if d, err := play(dovi); err != nil || d.Kind != core.DecisionDirectPlay {
		t.Fatalf("Dolby Vision profile 8: %+v %v", d, err)
	}
	// H.264 may only be SDR. HDR and an unclassified HEVC picture must
	// transcode instead of failing the plan.
	h264HDR := hdr
	h264HDR.Codec = "h264"
	h264HDR.Profile = "High 10"
	h264HDR.Level = 51
	if d, err := play(h264HDR); err != nil || d.Kind != core.DecisionTranscode || d.VideoCopy || d.VideoCodec != "h264" {
		t.Fatalf("HDR H.264: %+v %v", d, err)
	}
	unknown := core.StreamInfo{Codec: "hevc", Width: 1920, Height: 1080, FrameRate: 24}
	if d, err := play(unknown); err != nil || d.Kind != core.DecisionTranscode || d.VideoCodec != "h264" {
		t.Fatalf("unknown HEVC range: %+v %v", d, err)
	}
	profile7 := dovi
	profile7.DolbyVisionProfile = 7
	profile7.DolbyVisionCompatibilityID = 6
	if d, err := play(profile7); err != nil || d.Kind != core.DecisionTranscode || d.VideoCopy {
		t.Fatalf("Dolby Vision profile 7: %+v %v", d, err)
	}
}

// Jellyfin Android TV does not whitelist ranges. It applies an HEVC codec
// profile only when VideoRangeType is one of the unsupported types, then
// rejects that profile with NotEquals of the same pipe-separated list so the
// server transcodes. SDR never enters the profile and still direct-plays.
// Upstream Findroid sends an empty codec profile. Infuse has no captured
// codec profile in this repo. Findroid CE excludes Dolby Vision with the same
// VideoRangeType condition, which this comparison covers.
func TestAndroidTVUnsupportedRangeTranscodes(t *testing.T) {
	unsupported := "DOVI|DOVIWithEL|DOVIWithHDR10|DOVIWithHDR10Plus|HDR10|HDR10Plus"
	profiles := &core.ClientCapabilityProfiles{
		DirectPlay: []core.ClientDirectPlayProfile{{MediaType: "video", Containers: []string{"mp4"}, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac"}}},
		Codecs: []core.ClientCodecProfile{{
			MediaType:       "video",
			Codecs:          []string{"hevc"},
			ApplyConditions: []core.ClientProfileCondition{{Property: "VideoRangeType", Operator: "EqualsAny", Value: unsupported, Required: false}},
			Conditions:      []core.ClientProfileCondition{{Property: "VideoRangeType", Operator: "NotEquals", Value: unsupported, Required: false}},
		}},
		Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", Delivery: "hls", Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: "8"}},
	}
	caps := core.ClientCaps{Profiles: profiles, Containers: []string{"mp4"}, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac"}, HLSContainers: []string{"mp4"}}
	broker := &Broker{deps: BrokerDeps{Transcoder: profileTranscoder{}}}
	req := frontend.PlanRequest{Caps: caps, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: 1, SubtitleStream: -1}}}
	no := false
	play := func(video core.StreamInfo) (core.Decision, error) {
		t.Helper()
		video.Index = 0
		video.Type = "video"
		info := core.MediaInfo{Container: "mp4", BitrateKbps: 2500, Streams: []core.StreamInfo{video, {Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}
		return broker.decideProfile(&core.PlaySession{Info: info}, req)
	}
	sdr := core.StreamInfo{Codec: "hevc", Width: 1920, Height: 1080, FrameRate: 24, BitDepth: 8, PixelFormat: "yuv420p", DolbyVisionConfigurationPresent: &no, HDR10Plus: &no}
	if d, err := play(sdr); err != nil || d.Kind != core.DecisionDirectPlay {
		t.Fatalf("SDR HEVC: %+v %v", d, err)
	}
	hdr := sdr
	hdr.BitDepth = 10
	hdr.PixelFormat = "yuv420p10le"
	hdr.ColorTransfer = "smpte2084"
	if d, err := play(hdr); err != nil || d.Kind != core.DecisionTranscode || d.VideoCopy || d.VideoCodec != "h264" {
		t.Fatalf("unsupported HDR10: %+v %v", d, err)
	}
}
