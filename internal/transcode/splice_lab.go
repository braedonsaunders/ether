//go:build splicelab

package transcode

import (
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

// SpliceLabArgs exposes the shipping argument builder only in the offline lab.
type SpliceLabHardware struct {
	Accel             core.HWAccel
	HWAccels, Filters []string
	H264Level         int
	H264FrameRate     float64
}

func SpliceLabArgs(input, dir string, info core.MediaInfo, container string, first int, copyVideo bool) []string {
	return SpliceLabHardwareArgs(input, dir, info, container, first, copyVideo, SpliceLabHardware{})
}

// SpliceLabHardwareArgs uses the shipping hardware decision/feature graph without
// probing or running a GPU on the machine that emits this argument array.
func SpliceLabHardwareArgs(input, dir string, info core.MediaInfo, container string, first int, copyVideo bool, hw SpliceLabHardware) []string {
	s := runSpec{Input: input, Dir: dir, Info: info, Decision: core.Decision{Kind: core.DecisionTranscode, VideoCopy: copyVideo, AudioCopy: true, VideoCodec: "h264", AudioCodec: "aac", AudioStream: 1, Container: container, MaxHeight: 360, BitrateKbps: 1200, SubtitleStream: -1}, Start: time.Duration(first) * 4 * time.Second, StartSeg: first, SegDur: 4 * time.Second, Playlist: "index.m3u8", InitName: "init.mp4", Protocol: core.ProtocolHLS}
	s.Accel = hw.Accel
	s.Decision.H264Level = hw.H264Level
	s.Decision.H264FrameRate = hw.H264FrameRate
	s.Features.hwaccels = map[string]bool{}
	s.Features.filters = map[string]bool{}
	for _, n := range hw.HWAccels {
		s.Features.hwaccels[n] = true
	}
	for _, n := range hw.Filters {
		s.Features.filters[n] = true
	}
	return buildArgs(s)
}
