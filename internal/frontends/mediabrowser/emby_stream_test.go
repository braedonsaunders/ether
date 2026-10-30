package mediabrowser

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestEmbyStreamUsesKnownSampleAspectRatio(t *testing.T) {
	i := &instance{protocol: "emby"}
	streams := []frontend.StreamInfo{{Type: "video", Width: 720, Height: 480, SampleAspectRatio: "8:9"}, {Type: "video", Width: 1920, Height: 1080}}
	result := i.mediaStreams(streams)
	first := result[0].(map[string]any)
	if first["AspectRatio"] != "4:3" || first["IsAnamorphic"] != true {
		t.Fatalf("aspect ratio: %#v", first)
	}
	if _, ok := first["IsOriginal"]; ok {
		t.Fatal("Emby stream contains Jellyfin-only field")
	}
	if _, ok := result[1].(map[string]any)["AspectRatio"]; ok {
		t.Fatal("unknown aspect ratio was invented")
	}
	if _, ok := result[1].(map[string]any)["IsAnamorphic"]; ok {
		t.Fatal("unknown sample aspect ratio was treated as square")
	}
	if _, ok := (&instance{protocol: "jellyfin"}).mediaStreams(streams)[0].(map[string]any)["IsOriginal"]; !ok {
		t.Fatal("Jellyfin stream lost its field")
	}
}

func TestEmbyStreamUsesOptionalProbeFacts(t *testing.T) {
	avg, reference := 23.976, 24.0
	nal, refs, depth, level := 4, 1, 8, 41
	interlaced := false
	stream := frontend.StreamInfo{Type: "video", CodecTag: "avc1", TimeBase: "1/24000", PixelFormat: "yuv420p", ColorTransfer: "smpte2084", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc", DisplayAspectRatio: "16:9", SampleAspectRatio: "1:1", AverageFrameRate: &avg, ReferenceFrameRate: &reference, NALLengthSize: &nal, ReferenceFrames: &refs, BitsPerRawSample: &depth, ProbedLevel: &level, ProbedInterlaced: &interlaced}
	dto := (&instance{protocol: "emby"}).mediaStreams([]frontend.StreamInfo{stream})[0].(map[string]any)
	for key, want := range map[string]any{"CodecTag": "avc1", "TimeBase": "1/24000", "PixelFormat": "yuv420p", "ColorTransfer": "smpte2084", "ColorPrimaries": "bt2020", "ColorSpace": "bt2020nc", "AspectRatio": "16:9", "IsAnamorphic": false, "AverageFrameRate": avg, "RealFrameRate": reference, "NalLengthSize": "4", "RefFrames": 1, "BitDepth": 8, "Level": 41, "IsInterlaced": false} {
		if dto[key] != want {
			t.Fatalf("%s: got %#v, want %#v", key, dto[key], want)
		}
	}
	unknown := (&instance{protocol: "emby"}).mediaStreams([]frontend.StreamInfo{{Type: "video"}})[0].(map[string]any)
	for _, key := range []string{"CodecTag", "TimeBase", "PixelFormat", "ColorTransfer", "ColorPrimaries", "ColorSpace", "AspectRatio", "AverageFrameRate", "NalLengthSize", "RefFrames", "IsInterlaced"} {
		if _, ok := unknown[key]; ok {
			t.Fatalf("unknown %s was emitted: %#v", key, unknown)
		}
	}
	audio := (&instance{protocol: "emby"}).mediaStreams([]frontend.StreamInfo{{Type: "audio"}})[0].(map[string]any)
	if audio["IsInterlaced"] != false {
		t.Fatal("audio cannot be interlaced", audio)
	}
}
