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
	for key, want := range map[string]any{"AverageFrameRate": fps, "ReferenceFrameRate": fps, "AspectRatio": "16:9", "PixelFormat": "yuv420p10le", "CodecTag": "hvc1", "TimeBase": "1/90000", "NalLengthSize": "4", "RefFrames": 4, "BitDepth": 10, "Level": 153, "IsInterlaced": false, "ColorPrimaries": "bt2020"} {
		if data[key] != want {
			t.Errorf("%s: got %v want %v", key, data[key], want)
		}
	}
	unknown := i.mediaStreams([]frontend.StreamInfo{{Type: "video"}})[0].(map[string]any)
	for _, key := range []string{"AverageFrameRate", "ReferenceFrameRate", "AspectRatio", "PixelFormat", "CodecTag", "TimeBase", "NalLengthSize", "RefFrames", "BitDepth", "Level", "ColorPrimaries"} {
		if _, ok := unknown[key]; ok {
			t.Errorf("unknown %s exposed", key)
		}
	}
}
