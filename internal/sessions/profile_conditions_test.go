package sessions

import (
	"strconv"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestProfileConditionPropertiesAndOperators(t *testing.T) {
	yes := true
	no := false
	refs := 4
	zeroRefs := 0
	depth := 24
	nal := 4
	video := &core.StreamInfo{Type: "video", Width: 1920, Height: 1080, BitDepth: 10, Level: 51, Profile: "High", FrameRate: 23.976, BitrateKbps: 8000, CodecTag: "avc1", SampleAspectRatio: "1:1", ReferenceFrames: &refs, NALLengthSize: &nal, FieldOrder: "progressive"}
	wide := *video
	wide.SampleAspectRatio = "4:3"
	wide.ProbedInterlaced = &yes
	audio := core.StreamInfo{Index: 2, Type: "audio", Profile: "LC", Channels: 6, SampleRate: 48000, BitrateKbps: 640, BitsPerRawSample: &depth}
	info := core.MediaInfo{Container: "mp4", BitrateKbps: 9000, Streams: []core.StreamInfo{*video, {Index: 1, Type: "audio", Channels: 2}, audio}}
	cases := []struct {
		name           string
		cond           core.ClientProfileCondition
		stream         *core.StreamInfo
		media          core.MediaInfo
		match, unknown bool
	}{
		{"width equals", core.ClientProfileCondition{Property: "Width", Operator: "Equals", Value: "1920", Required: true}, video, info, true, false},
		{"width less", core.ClientProfileCondition{Property: "VideoWidth", Operator: "LessThanEqual", Value: "1280", Required: true}, video, info, false, false},
		{"width missing required", core.ClientProfileCondition{Property: "Width", Operator: "Equals", Value: "1920", Required: true}, &core.StreamInfo{Type: "video"}, info, false, true},
		{"width missing optional", core.ClientProfileCondition{Property: "Width", Operator: "Equals", Value: "1920"}, &core.StreamInfo{Type: "video"}, info, true, false},
		{"height greater", core.ClientProfileCondition{Property: "Height", Operator: "GreaterThanEqual", Value: "1080", Required: true}, video, info, true, false},
		{"bit depth equals any", core.ClientProfileCondition{Property: "VideoBitDepth", Operator: "EqualsAny", Value: "8|10", Required: true}, video, info, true, false},
		{"level less", core.ClientProfileCondition{Property: "VideoLevel", Operator: "LessThanEqual", Value: "51", Required: true}, video, info, true, false},
		{"profile not equals", core.ClientProfileCondition{Property: "VideoProfile", Operator: "NotEquals", Value: "Baseline", Required: true}, video, info, true, false},
		{"framerate less", core.ClientProfileCondition{Property: "VideoFramerate", Operator: "LessThanEqual", Value: "24", Required: true}, video, info, true, false},
		{"codec tag equals", core.ClientProfileCondition{Property: "VideoCodecTag", Operator: "Equals", Value: "AVC1", Required: true}, video, info, true, false},
		{"ref frames zero is known", core.ClientProfileCondition{Property: "RefFrames", Operator: "LessThanEqual", Value: "4", Required: true}, &core.StreamInfo{Type: "video", ReferenceFrames: &zeroRefs}, info, true, false},
		{"ref frames missing required", core.ClientProfileCondition{Property: "RefFrames", Operator: "Equals", Value: "4", Required: true}, videoWithoutRefs(), info, false, true},
		{"anamorphic false", core.ClientProfileCondition{Property: "IsAnamorphic", Operator: "Equals", Value: "false", Required: true}, video, info, true, false},
		{"anamorphic true", core.ClientProfileCondition{Property: "IsAnamorphic", Operator: "Equals", Value: "true", Required: true}, &wide, info, true, false},
		{"anamorphic unknown", core.ClientProfileCondition{Property: "IsAnamorphic", Operator: "Equals", Value: "false", Required: true}, &core.StreamInfo{Type: "video"}, info, false, true},
		{"interlaced progressive", core.ClientProfileCondition{Property: "IsInterlaced", Operator: "Equals", Value: "false", Required: true}, video, info, true, false},
		{"interlaced probed", core.ClientProfileCondition{Property: "IsInterlaced", Operator: "NotEquals", Value: "false", Required: true}, &wide, info, true, false},
		{"avc tag", core.ClientProfileCondition{Property: "IsAvc", Operator: "Equals", Value: "true", Required: true}, video, info, true, false},
		{"avc unknown", core.ClientProfileCondition{Property: "IsAvc", Operator: "Equals", Value: "true"}, &core.StreamInfo{Type: "video", Codec: "h264"}, info, true, false},
		{"audio channels", core.ClientProfileCondition{Property: "AudioChannels", Operator: "LessThanEqual", Value: "6", Required: true}, &audio, info, true, false},
		{"audio rate", core.ClientProfileCondition{Property: "AudioSampleRate", Operator: "Equals", Value: "48000", Required: true}, &audio, info, true, false},
		{"audio depth", core.ClientProfileCondition{Property: "AudioBitDepth", Operator: "GreaterThanEqual", Value: "16", Required: true}, &audio, info, true, false},
		{"audio depth missing", core.ClientProfileCondition{Property: "AudioBitDepth", Operator: "Equals", Value: "16", Required: true}, &core.StreamInfo{Type: "audio"}, info, false, true},
		{"audio profile", core.ClientProfileCondition{Property: "AudioProfile", Operator: "EqualsAny", Value: "HE-AAC|LC", Required: true}, &audio, info, true, false},
		{"secondary audio", core.ClientProfileCondition{Property: "IsSecondaryAudio", Operator: "Equals", Value: "true", Required: true}, &audio, info, true, false},
		{"primary audio", core.ClientProfileCondition{Property: "IsSecondaryAudio", Operator: "Equals", Value: "false", Required: true}, &info.Streams[1], info, true, false},
		{"audio stream count", core.ClientProfileCondition{Property: "NumAudioStreams", Operator: "Equals", Value: "2", Required: true}, video, info, true, false},
		{"video stream count zero", core.ClientProfileCondition{Property: "NumVideoStreams", Operator: "Equals", Value: "0", Required: true}, nil, core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio"}}}, true, false},
		{"bitrate", core.ClientProfileCondition{Property: "VideoBitrate", Operator: "LessThanEqual", Value: "8000000", Required: true}, video, info, true, false},
		{"container bitrate", core.ClientProfileCondition{Property: "ContainerBitrate", Operator: "Equals", Value: "9000000", Required: true}, video, info, true, false},
		{"packet length unknown", core.ClientProfileCondition{Property: "PacketLength", Operator: "Equals", Value: "188", Required: true}, video, info, false, true},
		{"packet length optional", core.ClientProfileCondition{Property: "PacketLength", Operator: "Equals", Value: "188"}, video, info, true, false},
		{"timestamp unknown", core.ClientProfileCondition{Property: "VideoTimestamp", Operator: "Equals", Value: "Valid", Required: true}, video, info, false, true},
		{"unknown operator", core.ClientProfileCondition{Property: "Width", Operator: "Between", Value: "1", Required: true}, video, info, false, true},
		{"unknown property", core.ClientProfileCondition{Property: "Future", Operator: "Equals", Value: "1", Required: true}, video, info, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			match, unknown := evaluateProfileConditions([]core.ClientProfileCondition{tc.cond}, tc.media, tc.stream)
			if match != tc.match || unknown != tc.unknown {
				t.Fatalf("match=%v unknown=%v", match, unknown)
			}
		})
	}
	hdr10Plus := core.StreamInfo{Type: "video", HDR10Plus: &yes, DolbyVisionConfigurationPresent: &no}
	match, unknown := evaluateProfileConditions([]core.ClientProfileCondition{{Property: "VideoRangeType", Operator: "Equals", Value: "HDR10", Required: true}}, core.MediaInfo{}, &hdr10Plus)
	if !match || unknown {
		t.Fatalf("HDR10Plus should satisfy HDR10, match=%v unknown=%v value=%s", match, unknown, profileVideoRangeValue(&hdr10Plus, core.ClientProfileCondition{Operator: "Equals", Value: "HDR10"}))
	}
}

func videoWithoutRefs() *core.StreamInfo {
	return &core.StreamInfo{Type: "video", Width: 100}
}

func TestProfileConditionNumericZeroStaysUnknown(t *testing.T) {
	match, unknown := evaluateProfileConditions([]core.ClientProfileCondition{{Property: "VideoLevel", Operator: "Equals", Value: strconv.Itoa(0), Required: true}}, core.MediaInfo{}, &core.StreamInfo{Type: "video", Level: 0, Width: 1920})
	if match || !unknown {
		t.Fatalf("unreported level match=%v unknown=%v", match, unknown)
	}
}
