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
		{"PQ", frontend.StreamInfo{ColorTransfer: "smpte2084"}, "hdr10"},
		{"HLG", frontend.StreamInfo{ColorTransfer: "arib-std-b67"}, "hlg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := siloDynamicRange(tc.stream); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
