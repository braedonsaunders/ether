package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// hdr10x265 encodes a 10-bit HDR10-signalled HEVC fixture. Differing x265
// params give the two releases different parameter sets, like a Blu-ray
// remux and a streaming WEB-DL of the same episode.
func hdr10x265(params string) []string {
	return []string{"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc",
		"-x265-params", "pools=1:frame-threads=1:keyint=24:min-keyint=24:scenecut=0:" + params,
		"-c:a", "flac"}
}

// TestIntegrationMatroskaFailoverKeepsOpenDecoder plays a segmented-Matroska
// session the way Plex's native player does: one EXT-X-MAP header fetched
// at startup, then every Cluster appended to the same demuxer input. A
// failover to a release with different parameter sets must decode with
// that original header; the player never refetches it.
func TestIntegrationMatroskaFailoverKeepsOpenDecoder(t *testing.T) {
	x264 := func(extra ...string) []string {
		return append([]string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-keyint_min", "24", "-sc_threshold", "0", "-c:a", "flac"}, extra...)
	}
	for _, tc := range []struct {
		name               string
		original, replaced []string
		hw, wantCopy       bool
	}{
		// Copy failover: same codec and range, different SPS/PPS.
		{"hevc-copy", hdr10x265("ctu=64:open-gop=0"), hdr10x265("ctu=32:no-sao=1:bframes=0:cbqpoffs=3"), false, true},
		{"h264-copy", x264(), x264("-bf", "0", "-profile:v", "baseline"), false, true},
		// A replacement that cannot be copied is re-encoded to the stream's codec.
		{"hevc-x265", hdr10x265("ctu=64:open-gop=0"), x264(), false, false},
		{"hevc-hardware", hdr10x265("ctu=64:open-gop=0"), x264(), true, false},
		{"h264-hardware", x264(), hdr10x265("ctu=32"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opt []Option
			if !tc.hw {
				opt = append(opt, WithoutHWDetection())
			}
			e := newRealEnv(t, opt...)
			e.gen(t, "remux.mkv", 24, tc.original...)
			e.gen(t, "web.mkv", 24, tc.replaced...)
			ctx := context.Background()
			info, err := e.m.Probe(ctx, e.url("remux.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			caps := appleTV
			caps.HLSContainers = []string{"mkv"}
			opts := allow()
			opts.DirectPlay = false
			d := e.m.Decide(*info, caps, opts)
			if d.Container != ContainerMatroska || !d.VideoCopy {
				t.Fatalf("decision: %+v", d)
			}
			hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: "it-mkv-failover-" + tc.name, InputURL: e.url("remux.mkv"), Info: *info, Decision: d})
			if err != nil {
				t.Fatal(err)
			}
			header := fetch(t, hs, InitSegment)
			joined := append([]byte(nil), header...)
			for n := 0; n < 2; n++ {
				joined = append(joined, fetch(t, hs, n)...)
			}
			other, err := e.m.Probe(ctx, e.url("web.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			if err := hs.SwapInput(ctx, e.url("web.mkv"), *other, 8*time.Second); err != nil {
				t.Fatal(err)
			}
			s := hs.(*Session)
			if got := s.Decision().VideoCopy; got != tc.wantCopy {
				t.Fatalf("failover video copy = %v, want %v", got, tc.wantCopy)
			}
			swapped := fetch(t, hs, 2)
			if tc.hw && !s.HWAccelerated() {
				t.Skip("no hardware encoder ran the failover")
			}
			if fresh := fetch(t, hs, InitSegment); bytes.Equal(fresh, header) {
				t.Fatal("replacement header equals the original; the fixture does not change parameter sets")
			}
			if n := e.inBandSPS(t, header, swapped); n == 0 {
				t.Fatal("replacement Cluster carries no in-band SPS")
			}
			joined = append(joined, swapped...)
			// Consume the complete fixture. A nonterminal segment may end
			// with reordered video whose corresponding audio is in the next
			// segment; truncating the playlist there is not an A/V gap.
			for n := 3; n < 6; n++ {
				joined = append(joined, fetch(t, hs, n)...)
			}
			path := e.writeTemp(t, "open-decoder.mkv", joined)
			if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-v", "error", "-xerror", "-err_detect", "explode", "-i", path, "-map", "0", "-f", "null", "-").CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) > 0 {
				t.Fatalf("original header + replacement clusters: %v\n%s", err, out)
			}
			frames := e.frameTimes(t, path, "v:0")
			if len(frames) < 6*4*24-2 {
				t.Fatalf("decoded %d video frames across six segments", len(frames))
			}
			for i := 1; i < len(frames); i++ {
				if frames[i] <= frames[i-1] || frames[i]-frames[i-1] > 0.1 {
					t.Fatalf("video timestamps jump %.3f -> %.3f at frame %d", frames[i-1], frames[i], i)
				}
			}
			audio := e.frameTimes(t, path, "a:0")
			if len(audio) > 0 {
				t.Logf("video: %d frames %.6f..%.6f; audio: %d frames %.6f..%.6f; hw=%v", len(frames), frames[0], frames[len(frames)-1], len(audio), audio[0], audio[len(audio)-1], s.HWAccelerated())
			}
			if len(audio) == 0 || audio[0] > frames[0]+0.1 || audio[len(audio)-1] < frames[len(frames)-1]-0.1 {
				t.Fatalf("audio does not span the video across the failover: %d frames", len(audio))
			}
			for i := 1; i < len(audio); i++ {
				// A restarted audio encoder may leave a short gap at the swap.
				if audio[i] <= audio[i-1] || audio[i]-audio[i-1] > 0.2 {
					t.Fatalf("audio timestamps jump %.3f -> %.3f", audio[i-1], audio[i])
				}
			}
		})
	}
}

// inBandSPS counts sequence parameter sets inside a Cluster's packets,
// excluding the header's CodecPrivate.
func (e *realEnv) inBandSPS(t *testing.T, header, cluster []byte) int {
	t.Helper()
	path := e.writeTemp(t, "sps-probe.mkv", append(append([]byte(nil), header...), cluster...))
	// trace_headers logs extradata first; the remaining SPS are in-band.
	out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-v", "error", "-i", path, "-map", "0:v:0", "-c", "copy",
		"-bsf:v", "trace_headers", "-loglevel", "trace", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("trace_headers: %v %s", err, out)
	}
	return bytes.Count(out, []byte("Sequence Parameter Set")) - bytes.Count(out, []byte("Extradata"))
}

// frameTimes decodes one stream of file and returns its frame timestamps.
func (e *realEnv) frameTimes(t *testing.T, file, stream string) []float64 {
	t.Helper()
	out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffprobe, "-v", "error", "-select_streams", stream, "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", file).Output()
	if err != nil {
		t.Fatalf("ffprobe frames %s: %v", stream, err)
	}
	var v struct {
		Frames []struct {
			Time string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	times := make([]float64, 0, len(v.Frames))
	for _, f := range v.Frames {
		at, err := strconv.ParseFloat(f.Time, 64)
		if err != nil {
			continue
		}
		times = append(times, at)
	}
	return times
}

func TestMatroskaHLSRepeatsParameterSetsInBand(t *testing.T) {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	copyMKV := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	mustContain(t, buildArgs(baseSpec(info, copyMKV)), "-c:v copy -bsf:v hevc_mp4toannexb")
	copyFMP4 := copyMKV
	copyFMP4.Container = ContainerFMP4
	mustNotContain(t, buildArgs(baseSpec(info, copyFMP4)), "mp4toannexb", "dump_extra")

	// The profile 7 base-layer chain is unchanged; parameter sets follow it.
	dv := mustParse(t, "mkv_dv_p8.json")
	dv.Video().DolbyVisionProfile = 7
	dv.Video().DolbyVisionCompatibilityID = 6
	caps := appleTV
	caps.HLSContainers = []string{ContainerMatroska}
	d := Decide(dv, caps, allow(), Policy{})
	mustContain(t, buildArgs(baseSpec(dv, d)), "-c:v copy -bsf:v dovi_rpu=strip=1,filter_units=remove_types=62|63,hevc_mp4toannexb")

	encode := core.Decision{Kind: core.DecisionTranscode, Container: ContainerMatroska, VideoCodec: "hevc", MaxHeight: 1080, AudioStream: 1, AudioCopy: true}
	args := buildArgs(baseSpec(info, encode))
	mustContain(t, args, "-bsf:v dump_extra=freq=keyframe")
	mustNotContain(t, args, "mp4toannexb")
	encode.Container = ContainerFMP4
	mustNotContain(t, buildArgs(baseSpec(info, encode)), "dump_extra")
}
