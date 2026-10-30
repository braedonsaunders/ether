package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"testing"
)

func TestJellyfinStreamDTOUsesOptionalProbeFacts(t *testing.T) {
	fps := 23.976
	refs, nal, bits, level := 4, 4, 10, 153
	progressive := false
	i := &instance{protocol: "jellyfin"}
	data := i.mediaStreams([]frontend.StreamInfo{{Type: "video", AverageFrameRate: &fps, ReferenceFrameRate: &fps, DisplayAspectRatio: "16:9", PixelFormat: "yuv420p10le", CodecTag: "hvc1", TimeBase: "1/90000", NALLengthSize: &nal, ReferenceFrames: &refs, BitsPerRawSample: &bits, ProbedLevel: &level, ProbedInterlaced: &progressive, ColorPrimaries: "bt2020"}})[0].(map[string]any)
	for key, want := range map[string]any{"AverageFrameRate": fps, "ReferenceFrameRate": fps, "RealFrameRate": fps, "AspectRatio": "16:9", "PixelFormat": "yuv420p10le", "CodecTag": "hvc1", "TimeBase": "1/90000", "NalLengthSize": "4", "RefFrames": 4, "BitDepth": 10, "Level": 153, "IsInterlaced": false, "ColorPrimaries": "bt2020", "AudioSpatialFormat": "None"} {
		if data[key] != want {
			t.Errorf("%s: got %v want %v", key, data[key], want)
		}
	}
	if _, ok := data["VideoRange"]; ok {
		t.Fatal("unclassified video range", data["VideoRange"])
	}
	unknown := i.mediaStreams([]frontend.StreamInfo{{Type: "video"}})[0].(map[string]any)
	for _, key := range []string{"AverageFrameRate", "ReferenceFrameRate", "AspectRatio", "PixelFormat", "CodecTag", "TimeBase", "NalLengthSize", "RefFrames", "BitDepth", "Level", "ColorPrimaries", "VideoRange", "IsAVC", "IsAnamorphic"} {
		if _, ok := unknown[key]; ok {
			t.Errorf("unknown %s exposed", key)
		}
	}
	if unknown["AudioSpatialFormat"] != "None" {
		t.Fatal(unknown["AudioSpatialFormat"])
	}
}

func TestJellyfinStreamRangeAndAudioLabels(t *testing.T) {
	no := false
	i := &instance{protocol: "jellyfin"}
	sdr := i.mediaStreams([]frontend.StreamInfo{{Type: "video", Codec: "h264", SampleAspectRatio: "1:1", DolbyVisionConfigurationPresent: &no, HDR10Plus: &no}})[0].(map[string]any)
	if sdr["VideoRange"] != "SDR" || sdr["VideoRangeType"] != "SDR" || sdr["IsAVC"] != true || sdr["IsAnamorphic"] != false {
		t.Fatal(sdr)
	}
	wide := i.mediaStreams([]frontend.StreamInfo{{Type: "video", Codec: "hevc", SampleAspectRatio: "8:9", ColorTransfer: "smpte2084"}})[0].(map[string]any)
	if wide["VideoRange"] != "HDR" || wide["VideoRangeType"] != "HDR10" || wide["IsAnamorphic"] != true || wide["IsAVC"] != false {
		t.Fatal(wide)
	}
	audio := i.mediaStreams([]frontend.StreamInfo{{Type: "audio", Language: "und", ChannelLayout: "5.1(side)", Title: "Atmos"}})[0].(map[string]any)
	for key, want := range map[string]any{"AudioSpatialFormat": "DolbyAtmos", "VideoRange": "Unknown", "VideoRangeType": "Unknown", "LocalizedLanguage": "Undetermined", "LocalizedDefault": "Default", "LocalizedExternal": "External", "LocalizedOriginal": "Original"} {
		if audio[key] != want {
			t.Errorf("%s: got %v", key, audio[key])
		}
	}
	named := i.mediaStreams([]frontend.StreamInfo{{Type: "audio", Language: "eng"}})[0].(map[string]any)
	if named["LocalizedLanguage"] != "English" {
		t.Fatal(named["LocalizedLanguage"])
	}
	text := i.mediaStreams([]frontend.StreamInfo{{Type: "subtitle", Codec: "subrip"}})[0].(map[string]any)
	image := i.mediaStreams([]frontend.StreamInfo{{Type: "subtitle", Codec: "hdmv_pgs_subtitle"}})[0].(map[string]any)
	if text["IsTextSubtitleStream"] != true || image["IsTextSubtitleStream"] != false {
		t.Fatal(text["IsTextSubtitleStream"], image["IsTextSubtitleStream"])
	}
}
