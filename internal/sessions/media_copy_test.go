package sessions

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPublicMediaDetachesDispositionAndVisionFlags(t *testing.T) {
	yes := true
	number := 4
	rate := 24.0
	source := core.MediaInfo{Streams: []core.StreamInfo{{Type: "subtitle", DefaultDisposition: &yes, ForcedDisposition: &yes, HearingImpaired: &yes, Original: &yes, VisualImpaired: &yes, Commentary: &yes, Dub: &yes, HDR10Plus: &yes, ProbedInterlaced: &yes, DolbyVisionConfigurationPresent: &yes, DolbyVisionBLCompatibilityIDPresent: &yes, NALLengthSize: &number, ReferenceFrames: &number, BitsPerRawSample: &number, ProbedLevel: &number, ProbedDolbyVisionProfile: &number, ProbedDolbyVisionLevel: &number, AverageFrameRate: &rate, ReferenceFrameRate: &rate, DolbyVisionBLPresent: &yes, ExternalURL: "secret"}}}
	visible := publicMedia(source)
	if visible.Streams[0].ExternalURL != "" {
		t.Fatal("source URL exposed")
	}
	*visible.Streams[0].HearingImpaired = false
	*visible.Streams[0].DefaultDisposition = false
	*visible.Streams[0].ForcedDisposition = false
	*visible.Streams[0].Original = false
	*visible.Streams[0].VisualImpaired = false
	*visible.Streams[0].Commentary = false
	*visible.Streams[0].Dub = false
	*visible.Streams[0].HDR10Plus = false
	*visible.Streams[0].ProbedInterlaced = false
	*visible.Streams[0].DolbyVisionConfigurationPresent = false
	*visible.Streams[0].DolbyVisionBLCompatibilityIDPresent = false
	*visible.Streams[0].NALLengthSize = 0
	*visible.Streams[0].ReferenceFrames = 0
	*visible.Streams[0].BitsPerRawSample = 0
	*visible.Streams[0].ProbedLevel = 0
	*visible.Streams[0].ProbedDolbyVisionProfile = 0
	*visible.Streams[0].ProbedDolbyVisionLevel = 0
	*visible.Streams[0].AverageFrameRate = 0
	*visible.Streams[0].ReferenceFrameRate = 0
	*visible.Streams[0].DolbyVisionBLPresent = false
	if !yes || number != 4 || rate != 24 {
		t.Fatal("returned stream flags mutated source")
	}
}
