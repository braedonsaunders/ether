package transcode

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestExplicitVideoTargetPreservesCodecPreferenceOrder(t *testing.T) {
	info := core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "mpeg4", Width: 1920, Height: 1080}, {Index: 1, Type: "audio", Codec: "aac", Channels: 2}}}
	for _, order := range [][]string{{"hevc", "h264"}, {"h264", "hevc"}} {
		caps := core.ClientCaps{VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac"}, TranscodeVideoCodecs: order}
		d := Decide(info, caps, core.DecideOptions{DirectPlay: false, DirectStream: false, AudioStream: 1, SubtitleStream: -1}, Policy{HEVCEncoder: true})
		if d.VideoCodec != order[0] {
			t.Fatalf("%v chose %s", order, d.VideoCodec)
		}
	}
}
