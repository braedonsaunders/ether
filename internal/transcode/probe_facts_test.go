package transcode

import "testing"

func TestProbeFactsPreserveUnknownAndExplicitValues(t *testing.T) {
	data := `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","codec_tag_string":"avc1","pix_fmt":"yuv420p","time_base":"1/90000","nal_length_size":"4","refs":0,"level":40,"bits_per_raw_sample":"8","display_aspect_ratio":"16:9","avg_frame_rate":"24000/1001","r_frame_rate":"24/1","field_order":"progressive","side_data_list":[{"dovi_config_present":0,"dv_bl_compat_id_present":0}]},{"index":1,"codec_type":"video","codec_name":"h264","field_order":"unknown","level":-99,"time_base":"0/0","avg_frame_rate":"0/0"}],"format":{}}`
	m, err := ParseProbe([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	a, b := m.Streams[0], m.Streams[1]
	if a.CodecTag != "avc1" || a.PixelFormat != "yuv420p" || a.TimeBase != "1/90000" || a.DisplayAspectRatio != "16:9" || a.NALLengthSize == nil || *a.NALLengthSize != 4 || a.ReferenceFrames == nil || *a.ReferenceFrames != 0 || a.ProbedLevel == nil || *a.ProbedLevel != 40 || a.BitsPerRawSample == nil || *a.BitsPerRawSample != 8 {
		t.Fatalf("missing known facts: %+v", a)
	}
	if a.AverageFrameRate == nil || *a.AverageFrameRate != 24000.0/1001 || a.ReferenceFrameRate == nil || *a.ReferenceFrameRate != 24 || a.ProbedInterlaced == nil || *a.ProbedInterlaced || a.DolbyVisionConfigurationPresent == nil || *a.DolbyVisionConfigurationPresent || a.DolbyVisionBLCompatibilityIDPresent == nil || *a.DolbyVisionBLCompatibilityIDPresent {
		t.Fatalf("missing known false or rate: %+v", a)
	}
	if b.Level != -99 || b.TimeBase != "" || b.AverageFrameRate != nil || b.ReferenceFrameRate != nil || b.ProbedInterlaced != nil || b.DolbyVisionConfigurationPresent != nil || b.DolbyVisionBLCompatibilityIDPresent != nil || b.ProbedLevel != nil {
		t.Fatalf("unknown became a claim: %+v", b)
	}
}

func TestDolbyVisionRecordTakesPrecedenceOverConflictingNegativeFlags(t *testing.T) {
	data := `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","side_data_list":[{"side_data_type":"DOVI configuration record","dovi_config_present":0,"dv_bl_signal_compatibility_id":0,"dv_bl_compat_id_present":0}]}],"format":{}}`
	m, err := ParseProbe([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	s := m.Streams[0]
	if s.DolbyVisionConfigurationPresent == nil || !*s.DolbyVisionConfigurationPresent || s.DolbyVisionBLCompatibilityIDPresent == nil || !*s.DolbyVisionBLCompatibilityIDPresent {
		t.Fatalf("positive evidence lost: %+v", s)
	}
}

func TestDolbyVisionContainerSignallingIsKnownOnlyAfterSupportedProbe(t *testing.T) {
	cases := []struct {
		name, format, tag, side string
		want                    *bool
		dv                      bool
	}{
		{name: "mp4 absent", format: "mov,mp4", tag: "hvc1", want: boolptr(false)},
		{name: "matroska absent", format: "matroska,webm", want: boolptr(false)},
		{name: "present", format: "mov,mp4", tag: "hvc1", side: `[{"side_data_type":"DOVI configuration record","dv_profile":8}]`, want: boolptr(true), dv: true},
		{name: "unsupported container", format: "mpegts", want: nil},
		{name: "incomplete container", format: "", want: nil},
		{name: "contradictory DV tag", format: "mov,mp4", tag: "dvhe", want: nil, dv: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			side := ""
			if tc.side != "" {
				side = `,"side_data_list":` + tc.side
			}
			data := `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","width":1920,"height":1080,"codec_tag_string":"` + tc.tag + `"` + side + `}],"format":{"format_name":"` + tc.format + `","duration":"60"}}`
			m, err := ParseProbe([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			s := m.Streams[0]
			if (s.DolbyVisionConfigurationPresent == nil) != (tc.want == nil) || (tc.want != nil && *s.DolbyVisionConfigurationPresent != *tc.want) || s.DolbyVision != tc.dv {
				t.Fatalf("container signalling: %+v", s)
			}
		})
	}
}

func TestPartialProbeDoesNotAssertAbsentDVConfiguration(t *testing.T) {
	data := `{"streams":[{"codec_type":"video","codec_name":"hevc"}],"format":{"format_name":"mov,mp4"}}`
	m, err := ParseProbe([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if m.Streams[0].DolbyVisionConfigurationPresent != nil {
		t.Fatal("partial probe asserted absence")
	}
}
func boolptr(v bool) *bool { return &v }
