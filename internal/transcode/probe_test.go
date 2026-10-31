package transcode

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ffprobe", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustParse(t *testing.T, name string) core.MediaInfo {
	t.Helper()
	m, err := ParseProbe(loadGolden(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return *m
}

func TestParseProbeGolden(t *testing.T) {
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	boolean := func(v bool) *bool { return &v }
	integer := func(v int) *int { return &v }
	rate := func(v float64) *float64 { return &v }
	tests := []struct {
		file string
		want core.MediaInfo
	}{
		{
			file: "mkv_hevc_hdr10.json",
			want: core.MediaInfo{
				Container: "mkv", Duration: sec(7950.125), Size: 59836712345, BitrateKbps: 60211,
				Streams: []core.StreamInfo{
					{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Level: 153, Default: true, Original: boolean(false), Commentary: boolean(false), Dub: boolean(false), Width: 3840, Height: 2160,
						FrameRate: 23.976, BitDepth: 10, ColorTransfer: "smpte2084", BitrateKbps: 58312,
						SampleAspectRatio: "1:1", ColorRange: "tv", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc"},
					{Index: 1, Type: "audio", Codec: "eac3", Language: "eng", Title: "English DD+ 5.1", Default: true,
						Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, BitrateKbps: 768},
					{Index: 2, Type: "audio", Codec: "ac3", Language: "fre", Title: "French AC3 5.1",
						Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, BitrateKbps: 448},
					{Index: 3, Type: "subtitle", Codec: "subrip", Language: "eng", Title: "Forced", Forced: true},
					{Index: 4, Type: "subtitle", Codec: "subrip", Language: "eng", Title: "SDH"},
				},
			},
		},
		{
			file: "mkv_dv_p8.json",
			want: core.MediaInfo{
				Container: "mkv", Duration: sec(7081), Size: 14812345678, BitrateKbps: 16734,
				Streams: []core.StreamInfo{
					{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Default: true, Width: 3840, Height: 1608,
						FrameRate: 23.976, BitDepth: 10, ColorTransfer: "smpte2084", DolbyVision: true, DolbyVisionProfile: 8, DolbyVisionLevel: 6, DolbyVisionCompatibilityID: 1,
						ColorRange: "tv", ColorPrimaries: "bt2020", ColorSpace: "bt2020nc",
						DolbyVisionVersion: "1.0", DolbyVisionBLPresent: boolean(true), DolbyVisionELPresent: boolean(false), DolbyVisionRPUPresent: boolean(true),
						BitrateKbps: 15876},
					{Index: 1, Type: "audio", Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Language: "eng",
						Default: true, Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, BitrateKbps: 768},
					{Index: 2, Type: "subtitle", Codec: "subrip", Language: "spa"},
				},
			},
		},
		{
			file: "mp4_h264_aac.json",
			want: core.MediaInfo{
				Container: "mp4", Duration: sec(5400.042), Size: 4062345678, BitrateKbps: 6018,
				Streams: []core.StreamInfo{
					{Index: 0, Type: "video", Codec: "h264", Profile: "High", Level: 40, Default: true, Width: 1920, Height: 1080,
						FrameRate: 24, BitDepth: 8, ColorTransfer: "bt709", BitrateKbps: 5823,
						ColorRange: "tv", ColorPrimaries: "bt709", ColorSpace: "bt709"},
					{Index: 1, Type: "audio", Codec: "aac", Profile: "LC", Language: "eng", Default: true,
						Channels: 2, ChannelLayout: "stereo", SampleRate: 48000, BitrateKbps: 192},
					{Index: 2, Type: "subtitle", Codec: "mov_text", Language: "eng"},
					// mjpeg cover art dropped
				},
			},
		},
		{
			file: "mkv_truehd_atmos.json",
			want: core.MediaInfo{
				Container: "mkv", Duration: sec(8123.456), Size: 36212345678, BitrateKbps: 35662,
				Streams: []core.StreamInfo{
					{Index: 0, Type: "video", Codec: "h264", Profile: "High", Default: true, Width: 1920, Height: 1080,
						FrameRate: 23.976, BitDepth: 8, ColorTransfer: "bt709", BitrateKbps: 28123},
					{Index: 1, Type: "audio", Codec: "truehd", Profile: "Dolby TrueHD + Dolby Atmos", Language: "eng",
						Title: "TrueHD Atmos 7.1", Default: true, Channels: 8, ChannelLayout: "7.1", SampleRate: 48000, BitrateKbps: 4512},
					{Index: 2, Type: "audio", Codec: "ac3", Language: "eng", Title: "AC3 5.1",
						Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000, BitrateKbps: 640},
					{Index: 3, Type: "audio", Codec: "dts", Profile: "DTS-HD MA", Language: "ger",
						Channels: 6, ChannelLayout: "5.1(side)", SampleRate: 48000},
				},
			},
		},
		{
			file: "mkv_pgs.json",
			want: core.MediaInfo{
				// no format bit_rate: derived from size/duration
				Container: "mkv", Duration: sec(1440), Size: 1440000000, BitrateKbps: 8000,
				Streams: []core.StreamInfo{
					{Index: 0, Type: "video", Codec: "h264", Profile: "High", Default: true, Width: 1920, Height: 800,
						FrameRate: 23.976, BitDepth: 8},
					{Index: 1, Type: "audio", Codec: "aac", Language: "jpn", Default: true,
						Channels: 2, ChannelLayout: "stereo", SampleRate: 48000},
					{Index: 2, Type: "subtitle", Codec: "pgs", Language: "eng", Default: true},
					{Index: 3, Type: "subtitle", Codec: "pgs", Language: "eng", Title: "Forced", Forced: true},
					{Index: 4, Type: "subtitle", Codec: "vobsub", Language: "fre"},
					{Index: 5, Type: "subtitle", Codec: "ass", Language: "ger"},
					// font attachment dropped
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			// Every retained stream in these recorded fixtures reports both
			// dispositions; preserve their presence as well as their values.
			for i := range tt.want.Streams {
				tt.want.Streams[i].DefaultDisposition = boolean(tt.want.Streams[i].Default)
				tt.want.Streams[i].ForcedDisposition = boolean(tt.want.Streams[i].Forced)
			}
			// Assert the additional probe facts without changing the legacy fields.
			v := &tt.want.Streams[0]
			if !v.DolbyVision && (tt.want.Container == "mp4" || tt.want.Container == "mkv") {
				v.DolbyVisionConfigurationPresent = boolean(false)
				if v.DolbyVisionBLCompatibilityIDPresent == nil {
					v.DolbyVisionBLCompatibilityIDPresent = boolean(false)
				}
			}
			switch tt.file {
			case "mkv_hevc_hdr10.json":
				v.ProbedLevel = integer(153)
				v.PixelFormat = "yuv420p10le"
				v.TimeBase = "1/1000"
				v.ReferenceFrames = integer(1)
				v.DisplayAspectRatio = "16:9"
				v.FieldOrder = "progressive"
				v.ProbedInterlaced = boolean(false)
				v.AverageFrameRate = rate(24000.0 / 1001)
				v.ReferenceFrameRate = rate(24000.0 / 1001)
				tt.want.Streams[1].TimeBase = "1/1000"
			case "mkv_dv_p8.json":
				v.PixelFormat = "yuv420p10le"
				v.AverageFrameRate = rate(24000.0 / 1001)
				v.ReferenceFrameRate = rate(24000.0 / 1001)
				v.DolbyVisionConfigurationPresent = boolean(true)
				v.DolbyVisionBLCompatibilityIDPresent = boolean(true)
				v.ProbedDolbyVisionProfile = integer(8)
				v.ProbedDolbyVisionLevel = integer(6)
			case "mp4_h264_aac.json":
				v.Language = "und"
				v.ProbedLevel = integer(40)
				v.PixelFormat = "yuv420p"
				v.CodecTag = "avc1"
				v.TimeBase = "1/12288"
				v.BitsPerRawSample = integer(8)
				v.AverageFrameRate = rate(24)
				v.ReferenceFrameRate = rate(24)
				tt.want.Streams[1].CodecTag = "mp4a"
				tt.want.Streams[2].CodecTag = "tx3g"
			case "mkv_truehd_atmos.json":
				v.PixelFormat = "yuv420p"
				v.AverageFrameRate = rate(24000.0 / 1001)
				v.ReferenceFrameRate = rate(24000.0 / 1001)
				tt.want.Streams[1].BitsPerRawSample = integer(24)
			case "mkv_pgs.json":
				v.PixelFormat = "yuv420p"
				v.ReferenceFrameRate = rate(24000.0 / 1001)
			}
			got := mustParse(t, tt.file)
			for i := range got.Streams {
				if i < len(tt.want.Streams) {
					tt.want.Streams[i].CodecLongName = got.Streams[i].CodecLongName
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				if len(got.Streams) != len(tt.want.Streams) {
					t.Fatalf("streams: got %d want %d\n%+v", len(got.Streams), len(tt.want.Streams), got.Streams)
				}
				for i := range got.Streams {
					if !reflect.DeepEqual(got.Streams[i], tt.want.Streams[i]) {
						t.Errorf("stream %d:\n got %+v\nwant %+v", i, got.Streams[i], tt.want.Streams[i])
					}
				}
				got.Streams, tt.want.Streams = nil, nil
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("format:\n got %+v\nwant %+v", got, tt.want)
				}
			}
		})
	}
}

func TestDynamicRangeFromGolden(t *testing.T) {
	cases := map[string]string{
		"mkv_hevc_hdr10.json": rangeHDR10,
		"mkv_dv_p8.json":      rangeDV,
		"mp4_h264_aac.json":   rangeSDR,
	}
	for f, want := range cases {
		info := mustParse(t, f)
		if got := dynamicRange(info.Video()); got != want {
			t.Errorf("%s: range %s, want %s", f, got, want)
		}
	}
	dv := mustParse(t, "mkv_dv_p8.json")
	if base, ok := dvBaseLayer(dv.Video()); !ok || base != rangeHDR10 {
		t.Errorf("DV p8.1 base = %q,%v; want hdr10", base, ok)
	}
}

func TestDolbyVisionFromCodecTag(t *testing.T) {
	js := `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","codec_tag_string":"dvh1","width":3840,"height":2160,"pix_fmt":"yuv420p10le"}],
	"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"10.0"}}`
	m, err := ParseProbe([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	v := m.Video()
	if !v.DolbyVision || v.DolbyVisionProfile != 0 {
		t.Fatalf("got DV=%v profile=%d", v.DolbyVision, v.DolbyVisionProfile)
	}
	if v.DolbyVisionVersion != "" || v.DolbyVisionBLPresent != nil || v.DolbyVisionELPresent != nil || v.DolbyVisionRPUPresent != nil {
		t.Fatal("codec tag alone invented a Dolby Vision configuration record")
	}
	if _, ok := dvBaseLayer(v); ok {
		t.Error("unknown-profile DV without transfer info must not be treated as having a base layer")
	}
	// HLG
	js = `{"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","color_transfer":"arib-std-b67","width":3840,"height":2160}],"format":{"format_name":"mpegts","duration":"10"}}`
	m, _ = ParseProbe([]byte(js))
	if dynamicRange(m.Video()) != rangeHLG || m.Container != "mpegts" {
		t.Errorf("HLG: %+v", m)
	}
}

func TestNormalizeContainer(t *testing.T) {
	cases := map[string]string{
		"matroska,webm":            "mkv",
		"mov,mp4,m4a,3gp,3g2,mj2":  "mp4",
		"mpegts":                   "mpegts",
		"avi":                      "avi",
		"asf":                      "wmv",
		"mpeg":                     "mpeg",
		"flv":                      "flv",
		"webm":                     "webm",
		"something_new":            "something_new",
		"matroska":                 "mkv",
		"mov,mp4,m4a,3gp,3g2,mj2 ": "mp4",
	}
	for in, want := range cases {
		if got := NormalizeContainer(strings.TrimSpace(in)); got != want {
			t.Errorf("NormalizeContainer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeCodec(t *testing.T) {
	cases := map[string]string{
		"hevc": "hevc", "h264": "h264", "av1": "av1", "vp9": "vp9", "dca": "dts", "truehd": "truehd",
		"hdmv_pgs_subtitle": "pgs", "dvd_subtitle": "vobsub", "subrip": "subrip", "ssa": "ass",
		"pcm_s24le": "pcm", "eac3": "eac3", "flac": "flac", "opus": "opus",
	}
	for in, want := range cases {
		if got := NormalizeCodec(in); got != want {
			t.Errorf("NormalizeCodec(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseProbeErrors(t *testing.T) {
	if _, err := ParseProbe([]byte("not json")); err == nil {
		t.Error("expected error for invalid JSON")
	}
	if _, err := ParseProbe([]byte(`{}`)); err == nil {
		t.Error("expected error for empty output")
	}
}

func TestParseProbeAudioHandlerName(t *testing.T) {
	raw := []byte(`{"format":{"format_name":"mov,mp4"},"streams":[
		{"codec_type":"audio","codec_name":"aac","tags":{"handler_name":"GPAC ISO Audio Handler"}},
		{"codec_type":"audio","codec_name":"aac","tags":{"title":"Commentary","handler_name":"SoundHandler"}},
		{"codec_type":"audio","codec_name":"aac","tags":{"handler_name":"SoundHandler"}},
		{"codec_type":"video","codec_name":"h264","tags":{"handler_name":"VideoHandler"}}
	]}`)
	info, err := ParseProbe(raw)
	if err != nil || len(info.Streams) != 4 {
		t.Fatal(err, info)
	}
	if info.Streams[0].Title != "GPAC ISO Audio Handler" || info.Streams[1].Title != "Commentary" || info.Streams[2].Title != "" || info.Streams[3].Title != "" {
		t.Fatal(info.Streams[0].Title, info.Streams[1].Title, info.Streams[2].Title, info.Streams[3].Title)
	}
}

func TestProbeRetriesLargerWhenIncomplete(t *testing.T) {
	incomplete := `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
		{"index":1,"codec_type":"audio","codec_name":"dts","channels":0,"sample_rate":"0"}],
		"format":{"format_name":"matroska,webm","duration":"100.0"}}`
	complete := string(loadGolden(t, "mkv_truehd_atmos.json"))
	fr := newFakeRunner()
	fr.probeJSON = []string{incomplete, complete}
	info, err := probe(context.Background(), fr, "/x/ffprobe", "http://127.0.0.1:1/src")
	if err != nil {
		t.Fatal(err)
	}
	if len(fr.probeCalls) != 2 {
		t.Fatalf("probe calls = %d, want 2", len(fr.probeCalls))
	}
	a, _ := strconv.Atoi(argValue(fr.probeCalls[0], "-probesize"))
	b, _ := strconv.Atoi(argValue(fr.probeCalls[1], "-probesize"))
	if a <= 0 || b <= a {
		t.Errorf("second probe should be larger: %d then %d", a, b)
	}
	first := fr.probeCalls[0]
	for _, want := range []string{"-print_format", "-show_format", "-show_streams", "-rw_timeout", "-analyzeduration"} {
		if !hasArg(first, want) {
			t.Errorf("probe args missing %s: %v", want, first)
		}
	}
	if first[len(first)-1] != "http://127.0.0.1:1/src" || first[len(first)-2] != "-i" {
		t.Errorf("input not last: %v", first)
	}
	if info.Streams[1].Codec != "truehd" {
		t.Errorf("got %+v", info.Streams[1])
	}
}

func TestProbeCompleteFirstTry(t *testing.T) {
	fr := newFakeRunner()
	fr.probeJSON = []string{string(loadGolden(t, "mp4_h264_aac.json"))}
	if _, err := probe(context.Background(), fr, "ffprobe", "http://h/x"); err != nil {
		t.Fatal(err)
	}
	if len(fr.probeCalls) != 1 {
		t.Errorf("probe calls = %d, want 1", len(fr.probeCalls))
	}
}

func TestProbeFailure(t *testing.T) {
	fr := newFakeRunner() // no JSON: every call fails
	_, err := probe(context.Background(), fr, "ffprobe", "http://h/x")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probe(ctx, fr, "ffprobe", "http://h/x"); err != context.Canceled {
		t.Errorf("cancelled probe err = %v", err)
	}
}
