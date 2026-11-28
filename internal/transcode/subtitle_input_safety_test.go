package transcode

import (
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSidecarSubtitleInputHasSafetyArgs(t *testing.T) {
	spec := runSpec{
		Input:    "http://127.0.0.1:8080/video.mkv",
		Protocol: core.ProtocolHTTP,
		Dir:      t.TempDir(),
		Info: core.MediaInfo{
			Container: "matroska",
			Duration:  120_000_000_000,
			Streams: []core.StreamInfo{
				{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
				{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
				{Index: 2, Type: "subtitle", Codec: "subrip", External: true, ExternalURL: "/tmp/sub.srt"},
			},
		},
		Decision: core.Decision{
			Kind:           core.DecisionTranscode,
			Container:      ContainerMatroska,
			VideoCopy:      true,
			AudioCopy:      true,
			SubtitleStream: 2,
		},
	}
	args := buildArgs(spec)
	idx := slices.Index(args, "/tmp/sub.srt")
	if idx < 5 {
		t.Fatalf("sidecar subtitle input missing from args: %v", args)
	}
	before := args[idx-5 : idx]
	if !slices.Contains(before, "-format_whitelist") || !slices.Contains(before, "-protocol_whitelist") {
		t.Fatalf("sidecar subtitle -i lacks whitelist args before it: %v", before)
	}
}
