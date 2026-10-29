package transcode

import "testing"

func TestHDR10PlusProbeIsTriState(t *testing.T) {
	for _, tc := range []struct {
		name, side   string
		known, value bool
	}{
		{"unknown", `[]`, false, false},
		{"dynamic metadata", `[{"side_data_type":"HDR Dynamic Metadata SMPTE2094-40"}]`, true, true},
		{"explicit negative", `[{"hdr10plus_flag":0}]`, true, false},
		{"explicit positive", `[{"hdr10plus_flag":1}]`, true, true},
		{"unrecognized flag", `[{"hdr10plus_flag":2}]`, false, false},
		{"metadata wins", `[{"hdr10plus_flag":0},{"side_data_type":"HDR Dynamic Metadata SMPTE2094-40"}]`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","side_data_list":` + tc.side + `}],"format":{}}`
			info, err := ParseProbe([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			got := info.Streams[0].HDR10Plus
			if (got != nil) != tc.known || (got != nil && *got != tc.value) {
				t.Fatalf("HDR10+ = %v, known=%t value=%t", got, tc.known, tc.value)
			}
		})
	}
}
