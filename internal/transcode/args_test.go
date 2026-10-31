package transcode

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func testFeatures() features {
	return features{
		scaleVTFormat: true,
		encoders:      parseEncoders([]byte(sampleEncoders)),
		filters:       parseFilters([]byte(sampleFilters)),
		hwaccels:      map[string]bool{"videotoolbox": true},
	}
}

func baseSpec(info core.MediaInfo, d core.Decision) runSpec {
	return runSpec{
		Input:    "http://127.0.0.1:32400/ether/source/1",
		Info:     info,
		Decision: d,
		Start:    200 * time.Second,
		SegDur:   4 * time.Second,
		StartSeg: 50,
		Dir:      "/tmp/sess",
		Playlist: "run-3.m3u8",
		InitName: "init-3.mp4",
		Accel:    core.HWAccel{Name: AccelNone},
		Features: testFeatures(),
	}
}

func TestVBVReservoirArgs(t *testing.T) {
	info := mustParse(t, "mkv_truehd_atmos.json")
	d := Decide(info, appleTV, allow(), Policy{})
	d.Kind = core.DecisionTranscode
	d.VideoCopy = false
	d.VideoCodec = "h264"
	s := baseSpec(info, d)
	s.Rate = rateControl{Quality: true, MaxrateKbps: 2400, BufsizeKbps: 19200, TargetKbps: 1800}
	args := buildArgs(s)
	mustContain(t, args, "-crf 18", "-maxrate 2400k", "-bufsize 19200k")
	mustNotContain(t, args, "-b:v 1800k")
}

func TestVBVHardwareQualityModes(t *testing.T) {
	info := mustParse(t, "mkv_truehd_atmos.json")
	d := Decide(info, appleTV, allow(), Policy{})
	d.Kind, d.VideoCopy, d.VideoCodec = core.DecisionTranscode, false, "h264"
	for _, tc := range []struct {
		accel string
		want  []string
	}{
		{AccelNVENC, []string{"-rc vbr", "-cq 21", "-maxrate 2400k", "-bufsize 19200k"}},
		{AccelVideoToolbox, []string{"-q:v 65", "-maxrate 2400k", "-bufsize 19200k"}},
		{AccelQSV, []string{"-global_quality 23", "-maxrate 2400k", "-bufsize 19200k"}},
		{AccelVAAPI, []string{"-rc_mode QVBR", "-global_quality 23", "-b:v 1800k", "-maxrate 2400k", "-bufsize 19200k"}},
	} {
		t.Run(tc.accel, func(t *testing.T) {
			s := baseSpec(info, d)
			s.Accel = core.HWAccel{Name: tc.accel, Encoders: []string{"h264_" + tc.accel}}
			s.Rate = rateControl{Quality: true, MaxrateKbps: 2400, BufsizeKbps: 19200, TargetKbps: 1800}
			mustContain(t, buildArgs(s), tc.want...)
		})
	}
}

func joined(args []string) string { return strings.Join(args, " ") }

func mustContain(t *testing.T, args []string, parts ...string) {
	t.Helper()
	s := joined(args)
	for _, p := range parts {
		if !strings.Contains(s, p) {
			t.Errorf("args missing %q\n%s", p, s)
		}
	}
}

func mustNotContain(t *testing.T, args []string, parts ...string) {
	t.Helper()
	s := joined(args)
	for _, p := range parts {
		if strings.Contains(s, p) {
			t.Errorf("args unexpectedly contain %q\n%s", p, s)
		}
	}
}

// inputIndex returns the position of "-i".
func inputIndex(args []string) int {
	for i, a := range args {
		if a == "-i" {
			return i
		}
	}
	return -1
}

// HEVC without libx265 and without a hardware HEVC encoder silently fell
// back to libx264; the fallback decision must now emit a structured Warn,
// and only for that decision.
func TestVideoEncoderHEVCWithoutLibx265WarnsFallback(t *testing.T) {
	noX265 := features{encoders: map[string]bool{"libx264": true}}
	if enc, hw := videoEncoder("hevc", core.HWAccel{Name: AccelNone}, noX265); enc != "libx264" || hw {
		t.Fatalf("videoEncoder(hevc, no libx265) = %q, hw=%v; want libx264, false", enc, hw)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if !warnVideoEncoderFallback(log, "hevc", core.HWAccel{Name: AccelNone}, noX265) {
		t.Fatal("warnVideoEncoderFallback = false, want true when it warns")
	}
	out := buf.String()
	for _, want := range []string{"level=WARN", "falling back to libx264", "requestedCodec=hevc", "encoder=libx264"} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback log %q missing %q", out, want)
		}
	}
	// No warning when HEVC is actually encodable, or H.264 was requested.
	for _, tc := range []struct {
		name  string
		codec string
		accel core.HWAccel
		feat  features
	}{
		{"libx265 present", "hevc", core.HWAccel{Name: AccelNone}, features{encoders: map[string]bool{"libx265": true}}},
		{"hw hevc present", "hevc", core.HWAccel{Name: AccelNVENC, Encoders: []string{"hevc_nvenc"}}, noX265},
		{"h264 requested", "h264", core.HWAccel{Name: AccelNone}, noX265},
	} {
		buf.Reset()
		if warnVideoEncoderFallback(log, tc.codec, tc.accel, tc.feat) {
			t.Errorf("%s: warned = true, want false", tc.name)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: unexpected log %q", tc.name, buf.String())
		}
	}
}

func TestBuildArgsRemuxTS(t *testing.T) {
	info := mustParse(t, "mkv_truehd_atmos.json")
	d := Decide(info, appleTV, allow(), Policy{})
	args := buildArgs(baseSpec(info, d))
	mustContain(t, args,
		"-reconnect 1", "-rw_timeout",
		"-start_at_zero -copyts -ss 200.000 -i http://127.0.0.1:32400/ether/source/1",
		"-map 0:0 -map 0:1",
		"-c:v copy",
		"-c:a eac3 -ac 6 -ar 48000 -b:a 640k",
		"-f hls -hls_time 4.000 -hls_list_size 0 -hls_playlist_type event -hls_flags temp_file -start_number 50",
		"-hls_segment_type mpegts",
		"-hls_segment_filename "+filepath.Join("/tmp/sess", "%05d.ts"),
		"-fflags +genpts",
		"-progress pipe:1",
	)
	mustNotContain(t, args, "-force_key_frames", "libx264", "-hwaccel", "hvc1", "-filter_complex")
	if args[len(args)-1] != filepath.Join("/tmp/sess", "run-3.m3u8") {
		t.Errorf("playlist must be last: %v", args[len(args)-1])
	}
	// -ss must be an input option.
	if i := inputIndex(args); i < 0 || argValue(args[:i], "-ss") != "200.000" {
		t.Errorf("-ss not before -i: %s", joined(args))
	}
}

func TestBuildArgsRemuxHEVCfMP4(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, iOS, allow(), Policy{})
	s := baseSpec(info, d)
	s.Start, s.StartSeg = 0, 0
	args := buildArgs(s)
	mustContain(t, args, "-c:v copy -tag:v hvc1", "-c:a copy", "-hls_segment_type fmp4 -hls_fmp4_init_filename "+func() string {
		if runtime.GOOS == "windows" {
			return filepath.Join(s.Dir, s.InitName)
		}
		return s.InitName
	}(),
		filepath.Join("/tmp/sess", "%05d.m4s"), "-start_number 0")
	mustNotContain(t, args, "-ss ")
}

func TestBuildArgsRemuxDolbyVisionKeepsConfig(t *testing.T) {
	info := mustParse(t, "mkv_dv_p8.json")
	d := Decide(info, iOS, allow(), Policy{})
	if d.Kind != core.DecisionRemux {
		t.Fatalf("decision %+v", d)
	}
	mustContain(t, buildArgs(baseSpec(info, d)), "-strict unofficial", "-tag:v hvc1")
}

func TestBuildArgsTranscodeCPUToneMap(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, plexWeb, allow(), Policy{})
	args := buildArgs(baseSpec(info, d))
	graph := argValue(args, "-filter_complex")
	for _, want := range []string{"[0:0]", "zscale=t=linear:npl=100", "tonemap=tonemap=hable", "scale=1920:1080", "format=yuv420p", "[vout]"} {
		if !strings.Contains(graph, want) {
			t.Errorf("graph %q missing %q", graph, want)
		}
	}
	// tone-map before scale
	if strings.Index(graph, "tonemap") > strings.Index(graph, "scale=1920") {
		t.Errorf("tone-map should precede scaling: %s", graph)
	}
	mustContain(t, args, "-map [vout]", "-c:v libx264", "-preset veryfast", "-force_key_frames expr:gte(t,n_forced*4.000)",
		"-color_trc bt709", "-c:a aac -ac 2", "-hls_segment_type mpegts")
	mustNotContain(t, args, "-hwaccel", "-fflags +genpts")
	vb := argValue(args, "-maxrate")
	if vb == "" || vb == "0k" {
		t.Errorf("no maxrate: %s", joined(args))
	}
}

func TestBuildArgsToneMapWithoutZscale(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, plexWeb, allow(), Policy{})
	s := baseSpec(info, d)
	s.Features.filters = map[string]bool{"scale": true, "overlay": true}
	graph := argValue(buildArgs(s), "-filter_complex")
	if strings.Contains(graph, "zscale") || !strings.Contains(graph, "setparams") {
		t.Errorf("fallback graph %q", graph)
	}
}

func TestBuildArgsVideoToolbox(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, plexWeb, allow(), Policy{})
	s := baseSpec(info, d)
	s.Accel = core.HWAccel{Name: AccelVideoToolbox, Encoders: []string{"h264_videotoolbox", "hevc_videotoolbox"}}
	if !s.usesHW() {
		t.Fatal("usesHW false")
	}
	args := buildArgs(s)
	mustContain(t, args, "-hwaccel videotoolbox -hwaccel_output_format videotoolbox_vld", "-c:v h264_videotoolbox", "-b:v",
		"scale_vt=w=1920:h=1080:color_matrix=bt709:color_primaries=bt709:color_transfer=bt709")
	mustNotContain(t, args, "zscale", "libx264", "-color_trc")
	// SDR scaling also keeps frames on the device.
	mp4 := mustParse(t, "mp4_h264_aac.json")
	o := allow()
	o.MaxHeight = 720
	d = Decide(mp4, appleTV, o, Policy{})
	s = baseSpec(mp4, d)
	s.Accel = core.HWAccel{Name: AccelVideoToolbox, Encoders: []string{"h264_videotoolbox"}}
	args = buildArgs(s)
	mustContain(t, args, "-hwaccel videotoolbox -hwaccel_output_format videotoolbox_vld", "scale_vt=w=1280:h=720:format=nv12", "-c:v h264_videotoolbox")
}

func TestBuildArgsVAAPI(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, plexWeb, allow(), Policy{})
	s := baseSpec(info, d)
	s.Accel = core.HWAccel{Name: AccelVAAPI, Encoders: []string{"h264_vaapi"}, Device: "/dev/dri/renderD129"}
	args := buildArgs(s)
	mustContain(t, args, "-hwaccel vaapi -hwaccel_device /dev/dri/renderD129 -hwaccel_output_format vaapi",
		"tonemap_vaapi=format=nv12:t=bt709:m=bt709:p=bt709,scale_vaapi=w=1920:h=1080:format=nv12", "-c:v h264_vaapi", "-rc_mode VBR")
	// Burn-in forces the software path with an upload at the end.
	pgs := mustParse(t, "mkv_pgs.json")
	o := allow()
	o.SubtitleStream = 2
	d = Decide(pgs, appleTV, o, Policy{})
	s = baseSpec(pgs, d)
	s.Accel = core.HWAccel{Name: AccelVAAPI, Encoders: []string{"h264_vaapi"}, Device: "/dev/dri/renderD128"}
	args = buildArgs(s)
	mustContain(t, args, "-vaapi_device /dev/dri/renderD128", "format=nv12,hwupload[vout]", "overlay")
}

func TestBuildArgsNVENCAndQSV(t *testing.T) {
	mp4 := mustParse(t, "mp4_h264_aac.json")
	o := allow()
	o.DirectPlay, o.DirectStream = false, false
	d := Decide(mp4, appleTV, o, Policy{})
	s := baseSpec(mp4, d)
	s.Accel = core.HWAccel{Name: AccelNVENC, Encoders: []string{"h264_nvenc"}}
	mustContain(t, buildArgs(s), "-hwaccel cuda", "-c:v h264_nvenc", "-forced-idr 1", "-rc vbr")
	s.Accel = core.HWAccel{Name: AccelQSV, Encoders: []string{"h264_qsv"}}
	mustContain(t, buildArgs(s), "-c:v h264_qsv", "format=nv12", "-look_ahead 0")
	// HEVC requested but only an h264 hw encoder: hevc goes to libx265.
	d.VideoCodec = "hevc"
	s.Decision = d
	if s.usesHW() {
		t.Error("hevc should not use the h264-only QSV encoder")
	}
	mustContain(t, buildArgs(s), "-c:v libx265", "-tag:v hvc1")
}

func TestBuildArgsKeepHDR(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, withRemote(appleTV), allow(), Policy{RemoteMaxBitrateKbps: 8000, HEVCEncoder: true})
	if d.VideoCodec != "hevc" || d.ToneMap {
		t.Fatalf("decision %+v", d)
	}
	args := buildArgs(baseSpec(info, d))
	mustContain(t, args, "-c:v libx265", "hdr10-opt=1", "transfer=smpte2084", "-color_trc smpte2084", "format=yuv420p10le",
		"-tag:v hvc1", "-hls_segment_type fmp4")
	mustNotContain(t, args, "tonemap")
}

func TestBuildArgsBurnPGS(t *testing.T) {
	pgs := mustParse(t, "mkv_pgs.json")
	o := allow()
	o.SubtitleStream = 2
	d := Decide(pgs, appleTV, o, Policy{})
	graph := argValue(buildArgs(baseSpec(pgs, d)), "-filter_complex")
	want := "[0:0]null[base];[0:2]scale=1920:800[sub];[base][sub]overlay=eof_action=pass,format=yuv420p[vout]"
	if graph != want {
		t.Errorf("graph\n got %s\nwant %s", graph, want)
	}
}

func TestBuildArgsTextSubtitleStaysSidecar(t *testing.T) {
	pgs := mustParse(t, "mkv_pgs.json")
	o := allow()
	o.SubtitleStream = 5
	o.BurnSubtitles = true
	d := Decide(pgs, appleTV, o, Policy{})
	if d.BurnSubtitles || !d.VideoCopy {
		t.Fatalf("text caused transcode: %+v", d)
	}
	if graph := argValue(buildArgs(baseSpec(pgs, d)), "-filter_complex"); graph != "" {
		t.Fatalf("unexpected graph %q", graph)
	}
}

func TestEscapeFilterValue(t *testing.T) {
	got := escapeFilterValue(`a:b'c,d[e]`)
	want := `a\\:b\\\'c\,d\[e\]` // per ffmpeg-filters "Notes on filtergraph escaping"
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestOutputSize(t *testing.T) {
	cases := []struct{ w, h, max, ww, wh int }{
		{3840, 2160, 1080, 1920, 1080},
		{3840, 1608, 1080, 2578, 1080},
		{1920, 1080, 0, 1920, 1080},
		{1920, 1080, 2160, 1920, 1080},
		{1921, 1081, 720, 1278, 720},
		{0, 0, 720, 1280, 720},
	}
	for _, c := range cases {
		w, h := outputSize(c.w, c.h, c.max)
		if w != c.ww || h != c.wh {
			t.Errorf("outputSize(%d,%d,%d) = %dx%d, want %dx%d", c.w, c.h, c.max, w, h, c.ww, c.wh)
		}
	}
}

func TestSubtitleArgs(t *testing.T) {
	args := subtitleArgs("http://h/x", 4)
	mustContain(t, args, "-i http://h/x -map 0:4 -c:s webvtt -f webvtt pipe:1")
}

func TestVideoBitrate(t *testing.T) {
	mp4 := mustParse(t, "mp4_h264_aac.json")
	d := core.Decision{Kind: core.DecisionTranscode, AudioStream: 1, AudioCopy: true, BitrateKbps: 4192, VideoCodec: "h264", MaxHeight: 720}
	if got := videoBitrate(mp4, d); got != 4000 {
		t.Errorf("videoBitrate = %d", got)
	}
}

func TestFMP4CopiedAudioPreloadScope(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	for _, tc := range []struct {
		name, container, accel string
		fps                    float64
		copyVideo, copyAudio   bool
		want                   string
	}{
		{"24fps", ContainerFMP4, AccelNone, 24, false, true, "83334"},
		{"60fps", ContainerFMP4, AccelNone, 60, false, true, "33334"},
		{"unknown fps", ContainerFMP4, AccelNone, 0, false, true, ""},
		{"copy", ContainerFMP4, AccelNone, 24, true, true, ""},
		{"encode audio", ContainerFMP4, AccelNone, 24, false, false, ""},
		{"ts", ContainerMPEGTS, AccelNone, 24, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSpec(info, core.Decision{Kind: core.DecisionTranscode, Container: tc.container, VideoCodec: "h264", VideoCopy: tc.copyVideo, AudioCopy: tc.copyAudio})
			s.Info.Streams = append([]core.StreamInfo(nil), info.Streams...)
			for i := range s.Info.Streams {
				if s.Info.Streams[i].Type == "video" {
					s.Info.Streams[i].FrameRate = tc.fps
				}
			}
			if got := argValue(buildArgs(s), "-audio_preload"); got != tc.want {
				t.Fatalf("preload=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestRestartedTSMarksTransportCounterReset(t *testing.T) {
	for _, container := range []string{ContainerMPEGTS, ContainerFMP4, ContainerMatroska} {
		for _, protocol := range []string{core.ProtocolHLS, core.ProtocolDASH} {
			for _, start := range []time.Duration{0, 8 * time.Second} {
				for _, rtx := range []bool{false, true} {
					s := runSpec{Decision: core.Decision{Container: container, VideoCopy: true, AudioStream: -1}, Protocol: protocol, Start: start, RTXTimeline: rtx}
					args := buildArgs(s)
					got := strings.Contains(argValue(args, "-hls_segment_options"), "mpegts_flags=+initial_discontinuity")
					want := container == ContainerMPEGTS && protocol == core.ProtocolHLS && start > 0
					if got != want {
						t.Fatalf("container=%s protocol=%s start=%s rtx=%v: reset marker=%v want=%v", container, protocol, start, rtx, got, want)
					}
					if want && rtx && !strings.Contains(argValue(args, "-hls_segment_options"), "mpegts_copyts=1:avoid_negative_ts=disabled") {
						t.Fatal("reset marker discarded RTX clock options")
					}
				}
			}
		}
	}
}

// Issue 13: with GPU scaling unavailable, VAAPI sessions decode, tone-map
// and scale in software and upload frames to the hardware encoder.
func TestBuildArgsVAAPIWithoutGPUScale(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, plexWeb, allow(), Policy{})
	s := baseSpec(info, d)
	s.Accel = core.HWAccel{Name: AccelVAAPI, Encoders: []string{"h264_vaapi"}, Device: "/dev/dri/renderD128", NoGPUScale: true}
	args := buildArgs(s)
	mustContain(t, args, "-vaapi_device /dev/dri/renderD128", "hwupload", "-c:v h264_vaapi")
	mustNotContain(t, args, "scale_vaapi", "tonemap_vaapi", "-hwaccel_output_format vaapi")
}

func TestBuildArgsVAAPIRCModesAndNoGPUTonemap(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	d := Decide(info, plexWeb, allow(), Policy{})
	d.BitrateKbps = 4192
	d.MaxHeight = 720

	// CQP-only driver: both VBV quality and explicit-bitrate runs use CQP with
	// a bitrate-scaled QP instead of failing on -rc_mode QVBR / VBR.
	s := baseSpec(info, d)
	s.Accel = core.HWAccel{
		Name:           AccelVAAPI,
		Encoders:       []string{"h264_vaapi"},
		Device:         "/dev/dri/renderD128",
		NoGPUScale:     true,
		VAAPIQualityRC: map[string]string{"h264_vaapi": "CQP"},
		VAAPIBitrateRC: map[string]string{"h264_vaapi": "CQP"},
	}
	s.Rate = rateControl{Quality: true, MaxrateKbps: 4000, BufsizeKbps: 32000, TargetKbps: 3600}
	qArgs := buildArgs(s)
	mustContain(t, qArgs, "-c:v h264_vaapi", "-rc_mode CQP", "-global_quality 23")
	mustNotContain(t, qArgs, "-rc_mode QVBR", "-rc_mode VBR", "-b:v", "-maxrate")

	s.Rate = rateControl{}
	bArgs := buildArgs(s)
	mustContain(t, bArgs, "-c:v h264_vaapi", "-rc_mode CQP", "-global_quality 22")

	// Tighter bitrate budget scales CQP upward so bandwidth-limited rungs stay bounded.
	sLow := s
	sLow.Decision.BitrateKbps = 1692 // 1500 kbps video
	lowArgs := buildArgs(sLow)
	mustContain(t, lowArgs, "-c:v h264_vaapi", "-rc_mode CQP", "-global_quality 30")
	mustNotContain(t, bArgs, "-rc_mode QVBR", "-rc_mode VBR", "-b:v", "-maxrate")

	// NoGPUTonemap: GPU scaler works, but HDR tone-mapping falls back to CPU
	// zscale/tonemap + hwupload instead of failing in tonemap_vaapi.
	sTonemap := baseSpec(info, d)
	sTonemap.Accel = core.HWAccel{
		Name:         AccelVAAPI,
		Encoders:     []string{"h264_vaapi"},
		Device:       "/dev/dri/renderD128",
		NoGPUTonemap: true,
	}
	tmArgs := buildArgs(sTonemap)
	mustContain(t, tmArgs, "-vaapi_device /dev/dri/renderD128", "tonemap=tonemap=hable", "hwupload", "-c:v h264_vaapi")
	mustNotContain(t, tmArgs, "tonemap_vaapi", "scale_vaapi")
}
