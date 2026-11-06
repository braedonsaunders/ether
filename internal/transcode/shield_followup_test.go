package transcode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Fake Troy source facts exercise the real encoder command builder, including
// the combined tone-map + subtitle path, rather than just the Plex XML row.
func TestShieldTroySubtitleEncoderPaths(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	info.Streams = append(info.Streams, core.StreamInfo{Index: 8, Type: "subtitle", Codec: "subrip", Language: "en"}, core.StreamInfo{Index: 9, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "en"})
	for _, mode := range []string{"copy", "tonemap"} {
		for _, sub := range []int{8, 9} {
			t.Run(fmt.Sprintf("%s/%d", mode, sub), func(t *testing.T) {
				d := core.Decision{Container: ContainerMatroska, Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: false, AudioCodec: "aac", AudioStream: 1, SubtitleStream: sub}
				if mode == "tonemap" || sub == 9 {
					d.Kind = core.DecisionTranscode
					d.VideoCopy = false
					d.VideoCodec = "h264"
					d.ToneMap = true
					d.MaxHeight = 1080
					d.BitrateKbps = 4000
				}
				d.BurnSubtitles = sub == 9
				args := buildArgs(baseSpec(info, d))
				mustContain(t, args, "-c:a aac")
				if sub == 8 {
					mustContain(t, args, "-map 0:8", "-c:s copy", "-disposition:s:0 default", "language=en")
					mustNotContain(t, args, "-sn")
				} else {
					mustContain(t, args, "[0:9]", "overlay")
				}
				if mode == "tonemap" {
					mustContain(t, args, "tonemap")
				}
				if mode == "copy" && sub == 8 {
					mustContain(t, args, "-c:v copy")
				}
			})
		}
	}
}

// Use real HEVC Main 10 HDR10 + DTS bytes for both SRT delivery paths. DV
// capability/RPU stripping and the PGS choice are covered by the fake-source
// HTTP/encoder contracts; this generated source carries no Dolby Vision RPU.
func TestIntegrationShieldHDRTextSegments(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	cues := filepath.Join(e.dir, "cues.srt")
	if err := os.WriteFile(cues, []byte(deliverySRT), 0600); err != nil {
		t.Fatal(err)
	}
	e.gen(t, "hdr-subs.mkv", 8, "-i", cues, "-map", "0:v", "-map", "1:a", "-map", "2", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=2:frame-threads=2:keyint=48:min-keyint=48:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc", "-vf", "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc", "-pix_fmt", "yuv420p10le", "-color_trc", "smpte2084", "-color_primaries", "bt2020", "-colorspace", "bt2020nc", "-c:a", "dca", "-strict", "-2", "-c:s", "copy", "-metadata:s:s:0", "language=eng")
	info, err := e.m.Probe(t.Context(), e.url("hdr-subs.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Video().Codec != "hevc" || info.Video().BitDepth != 10 || info.Video().ColorTransfer != "smpte2084" {
		t.Fatalf("source video: %+v", info.Video())
	}
	for _, tone := range []bool{false, true} {
		t.Run(fmt.Sprint(tone), func(t *testing.T) {
			if tone && !e.m.feat.filters["zscale"] {
				t.Skip("local FFmpeg lacks zscale; tone-map encoder graph covered by TestShieldTroySubtitleEncoderPaths")
			}
			d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: false, AudioCodec: "aac", AudioStream: 1, SubtitleStream: 2}
			if tone {
				d.Kind = core.DecisionTranscode
				d.VideoCopy = false
				d.ToneMap = true
				d.VideoCodec = "h264"
				d.MaxHeight = 360
				d.BitrateKbps = 1500
			}
			hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: fmt.Sprint("hdr-subs-", tone), InputURL: e.url("hdr-subs.mkv"), Info: *info, Decision: d})
			if err != nil {
				t.Fatal(err)
			}
			defer hs.Close()
			data := fetch(t, hs, InitSegment)
			data = append(data, fetch(t, hs, 0)...)
			data = append(data, fetch(t, hs, 1)...)
			path := e.writeTemp(t, fmt.Sprint("out-", tone, ".mkv"), data)
			streams := e.probeFile(t, path)
			if len(streams) != 3 || streams[1].CodecName != "aac" || streams[2].CodecName != "subrip" {
				t.Fatalf("output streams: %+v", streams)
			}
			want := "hevc"
			if tone {
				want = "h264"
			}
			if streams[0].CodecName != want {
				t.Fatalf("video: %+v", streams[0])
			}
			out, err := exec.Command(e.ffprobe, "-v", "error", "-select_streams", "s", "-show_entries", "packet=pts_time,duration_time", "-of", "json", path).Output()
			if err != nil {
				t.Fatal(err)
			}
			var packets struct {
				Packets []struct {
					PTS      string `json:"pts_time"`
					Duration string `json:"duration_time"`
				} `json:"packets"`
			}
			if err := json.Unmarshal(out, &packets); err != nil {
				t.Fatal(err)
			}
			if len(packets.Packets) == 0 || packets.Packets[0].Duration == "" {
				t.Fatalf("no timed subtitle cues: %s", out)
			}
		})
	}
}
