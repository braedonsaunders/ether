package transcode

import (
	"github.com/braedonsaunders/ether/internal/core"
	"strings"
	"testing"
)

func TestNativeHLSCodecAndHDRSignaling(t *testing.T) {
	for _, tc := range []struct {
		profile, compat     int
		codec, supplemental string
	}{
		{5, 0, "dvh1.05.06", ""},
		{8, 1, "hvc1.2.4.L150.90", "dvh1.08.06/db1p"},
		{8, 4, "hvc1.2.4.L150.90", "dvh1.08.06/db4h"},
	} {
		v := core.StreamInfo{Type: "video", Codec: "hevc", BitDepth: 10, DolbyVision: true, DolbyVisionProfile: tc.profile, DolbyVisionLevel: 6, DolbyVisionCompatibilityID: tc.compat, ColorTransfer: "smpte2084", Height: 2160}
		info := core.MediaInfo{Streams: []core.StreamInfo{v, {Type: "audio", Index: 1, Codec: "eac3"}}}
		dec := core.Decision{VideoCopy: true, AudioCopy: true, Container: ContainerFMP4, AudioStream: 1}
		attrs := HLSVariantAttributes(info, dec)
		if !strings.Contains(attrs, `CODECS="`+tc.codec+`,ec-3"`) {
			t.Fatal(attrs)
		}
		if tc.supplemental != "" && !strings.Contains(attrs, `SUPPLEMENTAL-CODECS="`+tc.supplemental+`"`) {
			t.Fatal(attrs)
		}
		videoRange := "PQ"
		if tc.compat == 4 {
			videoRange = "HLG"
		}
		if !strings.Contains(attrs, ",VIDEO-RANGE="+videoRange) {
			t.Fatal(attrs)
		}
		dec.VideoCopy = false
		dec.VideoCodec = "h264"
		dec.ToneMap = true
		attrs = HLSVariantAttributes(info, dec)
		if strings.Contains(attrs, "dvh1") || !strings.Contains(attrs, ",VIDEO-RANGE=SDR") {
			t.Fatal("source DV leaked into SDR transcode:", attrs)
		}
	}
}

func TestProbeCodecConfigurationHex(t *testing.T) {
	b := probeExtradata("\n00000000: 0164 0028 ffe1 0000                      .d.(....\n")
	if len(b) != 8 || b[1] != 0x64 || b[3] != 0x28 {
		t.Fatalf("extradata %x", b)
	}
	info, err := ParseProbe([]byte(`{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"extradata":"\n00000000: 0164 0028 ffe1 0000                      .d.(....\n"}]}`))
	if err != nil || info.Video().CodecString != "avc1.640028" {
		t.Fatalf("codec config: %+v %v", info, err)
	}
}

func TestHLSAttributesDoNotAdvertiseSourceDVOnConvertedOutput(t *testing.T) {
	info := mustParse(t, "mkv_dv_p8.json")
	info.Video().CodecString = "dvh1.08.06"
	for _, tc := range []struct {
		name     string
		decision core.Decision
	}{
		{"stripped HDR10", core.Decision{VideoCopy: true, StripDolbyVision: true}},
		{"encoded HEVC", core.Decision{VideoCodec: "hevc"}},
		{"encoded H264", core.Decision{VideoCodec: "h264", ToneMap: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := HLSVariantAttributes(info, tc.decision)
			if strings.Contains(attrs, "dvh1") || strings.Contains(attrs, "SUPPLEMENTAL-CODECS") {
				t.Fatal(attrs)
			}
			if tc.decision.VideoCopy && (!strings.Contains(attrs, `CODECS="hvc1.`) || !strings.Contains(attrs, "VIDEO-RANGE=PQ")) {
				t.Fatal("stripped output must advertise plain HEVC/PQ:", attrs)
			}
		})
	}
}
