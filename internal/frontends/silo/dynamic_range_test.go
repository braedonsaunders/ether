package silo

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestSiloDynamicRangeMixedAndUnknown(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name   string
		stream frontend.StreamInfo
		want   string
	}{
		{"unknown transfer", frontend.StreamInfo{}, ""},
		{"BT709 without enhancement evidence", frontend.StreamInfo{ColorTransfer: "bt709"}, ""},
		{"BT709 with HDR10+", frontend.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &yes}, "hdr10_plus"},
		{"BT709 with DV configuration", frontend.StreamInfo{ColorTransfer: "bt709", DolbyVisionConfigurationPresent: &yes}, "dolby_vision"},
		{"contradictory DV enhancement", frontend.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &no, DolbyVisionConfigurationPresent: &no, DolbyVisionRPUPresent: &no, DolbyVisionELPresent: &yes}, "dolby_vision"},
		{"known SDR", frontend.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &no, DolbyVisionConfigurationPresent: &no, DolbyVisionRPUPresent: &no}, "sdr"},
		{"untagged 8-bit 4:2:0", frontend.StreamInfo{BitDepth: 8, PixelFormat: "yuv420p", DolbyVisionConfigurationPresent: &no}, "sdr"},
		{"untagged without DV evidence", frontend.StreamInfo{BitDepth: 8, PixelFormat: "yuv420p"}, ""},
		{"PQ", frontend.StreamInfo{ColorTransfer: "smpte2084", BitDepth: 8, PixelFormat: "yuv420p", DolbyVisionConfigurationPresent: &no}, "hdr10"},
		{"HLG", frontend.StreamInfo{ColorTransfer: "arib-std-b67"}, "hlg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := siloDynamicRange(tc.stream); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSiloEstimatedRangeRemainsUnknown(t *testing.T) {
	yes := true
	stream := frontend.StreamInfo{ColorTransfer: "smpte2084", HDR10Plus: &yes}
	if got := siloObservedRange(frontend.MediaInfo{Estimated: true}, stream); got != "" {
		t.Fatalf("estimated range advertised: %q", got)
	}
	if got := siloObservedRange(frontend.MediaInfo{}, stream); got != "hdr10_plus" {
		t.Fatalf("probed range missing: %q", got)
	}
}
