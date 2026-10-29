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
