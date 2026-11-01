package transcode

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

var (
	appleTV = core.ClientCaps{
		Product: "Plex for Apple TV", Platform: "tvOS", MaxHeight: 2160, HDR10: true, DolbyVision: true,
		VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"},
		Containers: []string{"mp4", "mkv", "mpegts"},
	}
	iOS = core.ClientCaps{
		Product: "Plex for iOS", Platform: "iOS", MaxHeight: 2160, HDR10: true, DolbyVision: true,
		VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"},
		Containers: []string{"mp4", "mpegts"},
	}
	plexWeb = core.ClientCaps{
		Product: "Plex Web", Platform: "Chrome", MaxHeight: 1080,
		VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac", "mp3", "opus", "flac"},
		Containers: []string{"mp4"},
	}
	// Fixture with HDR10 already set — Decide behavior only. ClientCaps must
	// not infer HDR10 from hevc alone; that belongs in plex ClientCaps tests.
	plexWebModern = core.ClientCaps{
		Product: "Plex Web", Platform: "Chrome", MaxHeight: 2160, HDR10: true,
		VideoCodecs: []string{"h264", "vp9", "av1", "hevc"},
		AudioCodecs: []string{"aac", "mp3", "opus", "flac", "vorbis"},
		Containers:  []string{"mp4", "mkv", "webm", "mpegts"},
	}
	hdr10TV = core.ClientCaps{
		Product: "Plex for Android (TV)", MaxHeight: 2160, HDR10: true,
		VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"},
		Containers: []string{"mp4", "mkv", "mpegts"},
	}
)

func allow() core.DecideOptions {
	return core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}
}

func withRemote(c core.ClientCaps) core.ClientCaps { c.Remote = true; return c }

func TestDecide(t *testing.T) {
	pol := Policy{RemoteMaxBitrateKbps: 8000, HEVCEncoder: true}
	hdr := mustParse(t, "mkv_hevc_hdr10.json")
	dv8 := mustParse(t, "mkv_dv_p8.json")
	mp4 := mustParse(t, "mp4_h264_aac.json")
	truehd := mustParse(t, "mkv_truehd_atmos.json")
	pgs := mustParse(t, "mkv_pgs.json")
	dv5 := mustParse(t, "mkv_dv_p8.json")
	dv5.Streams[0].DolbyVisionProfile = 5
	dv5.Streams[0].ColorTransfer = ""

	type want struct {
		kind       core.DecisionKind
		videoCopy  bool
		audioCopy  bool
		videoCodec string
		audioCodec string
		container  string
		maxHeight  int
		toneMap    bool
		burn       bool
		audioIdx   int
		maxKbps    int // 0 = don't check; else BitrateKbps <= maxKbps
		reason     []string
	}
	tests := []struct {
		name string
		info core.MediaInfo
		caps core.ClientCaps
		opts func(*core.DecideOptions)
		pol  *Policy
		want want
	}{
		{
			name: "web direct plays mp4 h264 aac",
			info: mp4, caps: plexWeb,
			want: want{kind: core.DecisionDirectPlay, videoCopy: true, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: "mp4", maxHeight: 1080, audioIdx: 1, reason: []string{"direct play", "Plex Web"}},
		},
		{
			name: "apple tv direct plays 4K HDR10 mkv",
			info: hdr, caps: appleTV,
			want: want{kind: core.DecisionDirectPlay, videoCopy: true, audioCopy: true, videoCodec: "hevc", audioCodec: "eac3",
				container: "mkv", maxHeight: 2160, audioIdx: 1, reason: []string{"HDR10"}},
		},
		{
			name: "iOS remuxes mkv HEVC into fMP4 HLS",
			info: hdr, caps: iOS,
			want: want{kind: core.DecisionRemux, videoCopy: true, audioCopy: true, videoCodec: "hevc", audioCodec: "eac3",
				container: ContainerFMP4, maxHeight: 2160, audioIdx: 1, reason: []string{"container mkv not supported", "remux"}},
		},
		{
			name: "web transcodes HDR10 HEVC to SDR h264 1080p with stereo AAC",
			info: hdr, caps: plexWeb,
			want: want{kind: core.DecisionTranscode, videoCodec: "h264", audioCodec: "aac", container: ContainerMPEGTS,
				maxHeight: 1080, toneMap: true, audioIdx: 1, maxKbps: 10192,
				reason: []string{"tone-mapping HDR10", "video codec hevc not supported", "2160p exceeds 1080p"}},
		},
		{
			name: "modern web Direct Streams HDR10 HEVC (video copy, AAC remux)",
			info: hdr, caps: plexWebModern,
			want: want{kind: core.DecisionRemux, videoCopy: true, videoCodec: "hevc", audioCodec: "aac",
				container: ContainerFMP4, maxHeight: 2160, audioIdx: 1,
				reason: []string{"remux", "eac3", "hevc copied", "aac 2ch"}},
		},
		{
			name: "apple tv: TrueHD Atmos is transcoded to EAC3, video copied",
			info: truehd, caps: appleTV,
			want: want{kind: core.DecisionRemux, videoCopy: true, videoCodec: "h264", audioCodec: "eac3",
				container: ContainerMPEGTS, maxHeight: 1080, audioIdx: 1, reason: []string{"truehd", "eac3 6ch"}},
		},
		{
			name: "selected AC3 compat track copies",
			info: truehd, caps: iOS,
			opts: func(o *core.DecideOptions) { o.AudioStream = 2 },
			want: want{kind: core.DecisionRemux, videoCopy: true, audioCopy: true, videoCodec: "h264", audioCodec: "ac3",
				container: ContainerMPEGTS, maxHeight: 1080, audioIdx: 2},
		},
		{
			name: "DTS to web: stereo AAC",
			info: truehd, caps: plexWeb,
			opts: func(o *core.DecideOptions) { o.AudioStream = 3 },
			want: want{kind: core.DecisionRemux, videoCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 1080, audioIdx: 3, reason: []string{"dts", "aac 2ch"}},
		},
		{
			name: "DV profile 8.1 direct plays on HDR10-only TV via base layer",
			info: dv8, caps: hdr10TV,
			want: want{kind: core.DecisionDirectPlay, videoCopy: true, audioCopy: true, videoCodec: "hevc", audioCodec: "eac3",
				container: "mkv", maxHeight: 1608, audioIdx: 1},
		},
		{
			name: "DV profile 5 needs a tone-mapping transcode on HDR10-only TV",
			info: dv5, caps: hdr10TV,
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "eac3",
				container: ContainerMPEGTS, toneMap: true, audioIdx: 1, reason: []string{"profile 5"}},
		},
		{
			name: "remote client over the remote bitrate cap gets 720p h264",
			info: mp4, caps: withRemote(appleTV), pol: &Policy{RemoteMaxBitrateKbps: 4000},
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 720, audioIdx: 1, maxKbps: 4000, reason: []string{"exceeds 4000 kbps"}},
		},
		{
			name: "remote 4K HDR to apple tv keeps HDR in HEVC under the cap",
			info: hdr, caps: withRemote(appleTV),
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "hevc", audioCodec: "eac3",
				container: ContainerFMP4, maxHeight: 1080, audioIdx: 1, maxKbps: 8000, reason: []string{"keeping HDR"}},
		},
		{
			name: "remote 4K HDR without an HEVC encoder tone-maps to h264",
			info: hdr, caps: withRemote(appleTV), pol: &Policy{RemoteMaxBitrateKbps: 8000},
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "eac3",
				container: ContainerMPEGTS, maxHeight: 1080, toneMap: true, audioIdx: 1, maxKbps: 8000},
		},
		{
			name: "local client is not bound by the remote cap",
			info: hdr, caps: appleTV, pol: &Policy{RemoteMaxBitrateKbps: 1000},
			want: want{kind: core.DecisionDirectPlay, videoCopy: true, audioCopy: true, videoCodec: "hevc", audioCodec: "eac3",
				container: "mkv", maxHeight: 2160, audioIdx: 1},
		},
		{
			name: "client bitrate request forces transcode",
			info: mp4, caps: appleTV,
			opts: func(o *core.DecideOptions) { o.MaxBitrateKbps = 2000 },
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 480, audioIdx: 1, maxKbps: 2000},
		},
		{
			name: "videoResolution request caps height",
			info: mp4, caps: appleTV,
			opts: func(o *core.DecideOptions) { o.MaxHeight = 720 },
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 720, audioIdx: 1, maxKbps: 4192, reason: []string{"1080p exceeds 720p"}},
		},
		{
			name: "PGS subtitles are burned in",
			info: pgs, caps: appleTV,
			opts: func(o *core.DecideOptions) { o.SubtitleStream = 2 },
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 800, burn: true, audioIdx: 1, reason: []string{"pgs subtitles must be burned in"}},
		},
		{
			name: "VobSub subtitles are burned in",
			info: pgs, caps: appleTV,
			opts: func(o *core.DecideOptions) { o.SubtitleStream = 4 },
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 800, burn: true, audioIdx: 1},
		},
		{
			name: "text subtitles stay sidecar: direct play",
			info: pgs, caps: appleTV,
			opts: func(o *core.DecideOptions) { o.SubtitleStream = 5 },
			want: want{kind: core.DecisionDirectPlay, videoCopy: true, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: "mkv", maxHeight: 800, audioIdx: 1},
		},
		{
			name: "plex web burns PGS and direct plays text",
			info: pgs, caps: plexWebModern,
			opts: func(o *core.DecideOptions) { o.SubtitleStream = 2 },
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 800, burn: true, audioIdx: 1, reason: []string{"pgs subtitles must be burned in"}},
		},
		{
			name: "plex web text subtitle stays a direct-play sidecar",
			info: pgs, caps: plexWebModern,
			opts: func(o *core.DecideOptions) { o.SubtitleStream = 5 },
			want: want{kind: core.DecisionDirectPlay, videoCopy: true, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: "mkv", maxHeight: 800, audioIdx: 1},
		},
		{
			name: "android burns PGS",
			info: pgs, caps: core.ClientCaps{
				Product: "Plex for Android (Mobile)", Platform: "Android", MaxHeight: 1080,
				VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"},
				Containers: []string{"mp4", "mkv", "mpegts"},
			},
			opts: func(o *core.DecideOptions) { o.SubtitleStream = 2 },
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 800, burn: true, audioIdx: 1, reason: []string{"pgs subtitles must be burned in"}},
		},
		{
			name: "legacy text burn request stays sidecar",
			info: pgs, caps: appleTV,
			opts: func(o *core.DecideOptions) { o.SubtitleStream = 5; o.BurnSubtitles = true },
			want: want{kind: core.DecisionDirectPlay, videoCopy: true, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: "mkv", maxHeight: 800, audioIdx: 1},
		},
		{
			name: "direct play disallowed: remux",
			info: mp4, caps: appleTV,
			opts: func(o *core.DecideOptions) { o.DirectPlay = false },
			want: want{kind: core.DecisionRemux, videoCopy: true, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 1080, audioIdx: 1, reason: []string{"direct play not allowed"}},
		},
		{
			name: "direct play and stream disallowed: transcode",
			info: mp4, caps: appleTV,
			opts: func(o *core.DecideOptions) { o.DirectPlay = false; o.DirectStream = false },
			want: want{kind: core.DecisionTranscode, audioCopy: true, videoCodec: "h264", audioCodec: "aac",
				container: ContainerMPEGTS, maxHeight: 1080, audioIdx: 1, maxKbps: 6018 + 192,
				reason: []string{"direct stream not allowed"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := allow()
			if tt.opts != nil {
				tt.opts(&opts)
			}
			p := pol
			if tt.pol != nil {
				p = *tt.pol
			}
			d := Decide(tt.info, tt.caps, opts, p)
			w := tt.want
			check := func(field string, got, want any) {
				if got != want {
					t.Errorf("%s = %v, want %v (reason: %s)", field, got, want, d.Reason)
				}
			}
			check("Kind", d.Kind, w.kind)
			check("VideoCopy", d.VideoCopy, w.videoCopy)
			check("AudioCopy", d.AudioCopy, w.audioCopy)
			check("VideoCodec", d.VideoCodec, w.videoCodec)
			check("AudioCodec", d.AudioCodec, w.audioCodec)
			check("Container", d.Container, w.container)
			if w.maxHeight != 0 {
				check("MaxHeight", d.MaxHeight, w.maxHeight)
			}
			check("ToneMap", d.ToneMap, w.toneMap)
			check("BurnSubtitles", d.BurnSubtitles, w.burn)
			check("AudioStream", d.AudioStream, w.audioIdx)
			if w.maxKbps > 0 && (d.BitrateKbps > w.maxKbps || d.BitrateKbps <= 0) {
				t.Errorf("BitrateKbps = %d, want (0, %d]", d.BitrateKbps, w.maxKbps)
			}
			for _, r := range w.reason {
				if !strings.Contains(d.Reason, r) {
					t.Errorf("Reason %q missing %q", d.Reason, r)
				}
			}
			if d.Reason == "" {
				t.Error("empty Reason")
			}
		})
	}
}

func TestDecideSubtitleStreamIndex(t *testing.T) {
	pgs := mustParse(t, "mkv_pgs.json")
	opts := allow()
	opts.SubtitleStream = 0 // the video stream: not a subtitle, so none
	d := Decide(pgs, appleTV, opts, Policy{})
	if d.SubtitleStream != -1 || d.Kind != core.DecisionDirectPlay {
		t.Errorf("got %+v", d)
	}
	opts.SubtitleStream = 3
	d = Decide(pgs, appleTV, opts, Policy{})
	if d.SubtitleStream != 3 || !d.BurnSubtitles {
		t.Errorf("got %+v", d)
	}
}

func TestDecideLadder(t *testing.T) {
	cases := []struct {
		kbps  int
		codec string
		want  int
	}{
		{40000, "h264", 2160}, {8000, "h264", 1080}, {4000, "h264", 720}, {1500, "h264", 480}, {500, "h264", 360},
		{10000, "hevc", 2160}, {3000, "hevc", 1080},
	}
	for _, c := range cases {
		if got := heightForBitrate(c.kbps, c.codec); got != c.want {
			t.Errorf("heightForBitrate(%d,%s) = %d, want %d", c.kbps, c.codec, got, c.want)
		}
	}
	if bitrateCeiling(1080, "h264") <= bitrateCeiling(720, "h264") || bitrateCeiling(1080, "hevc") >= bitrateCeiling(1080, "h264") {
		t.Error("ceilings out of order")
	}
}

func TestAudioTarget(t *testing.T) {
	surround := &core.StreamInfo{Codec: "truehd", Channels: 8}
	stereo := &core.StreamInfo{Codec: "flac", Channels: 2}
	if got := audioTarget(appleTV, surround); got != "eac3" {
		t.Errorf("apple tv surround -> %s", got)
	}
	ac3Only := core.ClientCaps{AudioCodecs: []string{"aac", "ac3"}}
	if got := audioTarget(ac3Only, surround); got != "ac3" {
		t.Errorf("ac3-only surround -> %s", got)
	}
	if got := audioTarget(plexWeb, surround); got != "aac" {
		t.Errorf("web surround -> %s", got)
	}
	if got := audioTarget(appleTV, stereo); got != "aac" {
		t.Errorf("stereo -> %s", got)
	}
	if outputChannels("eac3", surround) != 6 || outputChannels("aac", surround) != 2 {
		t.Error("channel mapping")
	}
}
