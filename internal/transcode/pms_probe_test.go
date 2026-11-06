package transcode

import "testing"

func TestPMSProbeCoverArtAndEnhancementLayers(t *testing.T) {
	m, err := ParseProbe([]byte(`{"format":{"format_name":"matroska","bit_rate":"12345500"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","width":3840,"height":2160,"bit_rate":"10000500"},{"index":1,"codec_type":"video","codec_name":"hevc","width":1920,"height":1080,"side_data_list":[{"side_data_type":"DOVI configuration record","dv_profile":7,"bl_present_flag":0,"el_present_flag":1,"rpu_present_flag":1}]},{"index":2,"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1}},{"index":3,"codec_type":"video","codec_name":"h264","disposition":{"attached_pic":1}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 2 {
		t.Fatalf("cover art leaked or enhancement layer dropped: %+v", m.Streams)
	}
	el := m.Streams[1]
	if !el.DolbyVision || el.DolbyVisionProfile != 7 || el.DolbyVisionBLPresent == nil || *el.DolbyVisionBLPresent || el.DolbyVisionELPresent == nil || !*el.DolbyVisionELPresent {
		t.Fatalf("layer flags: %+v", el)
	}
	if m.BitrateKbps != 12346 || m.Streams[0].BitrateKbps != 10001 {
		t.Fatalf("nearest kbps: %d, %d", m.BitrateKbps, m.Streams[0].BitrateKbps)
	}
}
