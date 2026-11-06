package transcode

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// The two ways a converted stream shows an embedded text subtitle (#191):
// Matroska segments carry the picked track for a player that draws it
// (Plex for Android and Android TV), and a client that cannot draw it
// (Plex Web with an ASS track) gets it burned into the picture, from the
// start and after a seek.

// Cues at 1-4 s, 12-15 s and 21-24 s. The ASS style is an opaque white box
// (BorderStyle 3), so a burned cue changes the bottom of the frame plainly.
const (
	deliverySRT = "1\n00:00:01,000 --> 00:00:04,000\nOne\n\n2\n00:00:12,000 --> 00:00:15,000\nTwo\n\n3\n00:00:21,000 --> 00:00:24,000\nThree\n"
	deliveryASS = "[Script Info]\nScriptType: v4.00+\nPlayResX: 640\nPlayResY: 360\n\n[V4+ Styles]\n" +
		"Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n" +
		"Style: Default,Arial,48,&H00FFFFFF,&H000000FF,&H00FFFFFF,&H00FFFFFF,0,0,0,0,100,100,0,0,3,12,0,2,10,10,20,1\n\n" +
		"[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,WWWWWWWW\nDialogue: 0,0:00:12.00,0:00:15.00,Default,,0,0,0,,WWWWWWWW\nDialogue: 0,0:00:21.00,0:00:24.00,Default,,0,0,0,,WWWWWWWW\n"
)

// genSubtitled writes subtitled.mkv: video 0, audio 1, an English SRT (2)
// and a German ASS (3), neither flagged default, as releases often leave
// the track a viewer picks.
func genSubtitled(t *testing.T, e *realEnv) *core.MediaInfo {
	t.Helper()
	srt, ass := filepath.Join(e.dir, "cues.srt"), filepath.Join(e.dir, "cues.ass")
	if os.WriteFile(srt, []byte(deliverySRT), 0o644) != nil || os.WriteFile(ass, []byte(deliveryASS), 0o644) != nil {
		t.Fatal("write cues")
	}
	e.gen(t, "subtitled.mkv", 30, "-i", srt, "-i", ass, "-map", "0:v", "-map", "1:a", "-map", "2", "-map", "3",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "48", "-c:a", "aac", "-c:s", "copy",
		"-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=ger", "-disposition:s:0", "0", "-disposition:s:1", "0")
	info, err := e.m.Probe(t.Context(), e.url("subtitled.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// The Matroska segments of a Direct Stream carry exactly the picked text
// track, flagged default and with its language (ExoPlayer selects a text
// track by either; the release's own flags left it off), an ASS track
// with its script header, and its cues on the title clock.
func TestIntegrationMatroskaSegmentsCarryThePickedTextTrack(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithSegmentTimeout(20*time.Second))
	info := genSubtitled(t, e)
	for _, c := range []struct {
		index       int
		codec, lang string
	}{{2, "subrip", "eng"}, {3, "ass", "ger"}} {
		d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1, SubtitleStream: c.index}
		hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "picked-" + c.codec, InputURL: e.url("subtitled.mkv"), Info: *info, Decision: d})
		if err != nil {
			t.Fatal(err)
		}
		data := fetch(t, hs, InitSegment)
		for n := 0; n < 4; n++ {
			data = append(data, fetch(t, hs, n)...)
		}
		_ = hs.Close()
		path := e.writeTemp(t, "picked-"+c.codec+".mkv", data)
		out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffprobe, "-v", "error", "-print_format", "json", "-select_streams", "s",
			"-show_entries", "stream=codec_name,extradata_size:stream_disposition=default:stream_tags=language:packet=pts_time", path).Output()
		if err != nil {
			t.Fatalf("ffprobe: %v", err)
		}
		var got struct {
			Streams []struct {
				Codec       string            `json:"codec_name"`
				Extradata   int               `json:"extradata_size"`
				Disposition map[string]int    `json:"disposition"`
				Tags        map[string]string `json:"tags"`
			} `json:"streams"`
			Packets []struct {
				PTS string `json:"pts_time"`
			} `json:"packets"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Streams) != 1 {
			t.Fatalf("%s: %d subtitle tracks in the segments, want the picked one", c.codec, len(got.Streams))
		}
		s := got.Streams[0]
		if s.Codec != c.codec || s.Disposition["default"] != 1 || s.Tags["language"] != c.lang || (c.codec == "ass" && s.Extradata == 0) {
			t.Fatalf("%s track: %+v", c.codec, s)
		}
		// The cue at 1 s sits at 11 s: matroskaOrigin places the title.
		if len(got.Packets) == 0 {
			t.Fatalf("%s: no cues in the segments", c.codec)
		}
		if first, _ := strconv.ParseFloat(got.Packets[0].PTS, 64); math.Abs(first-(1+matroskaOrigin.Seconds())) > 0.05 {
			t.Fatalf("%s: first cue at %.3f", c.codec, first)
		}
	}
}

// A burned ASS cue is in the picture when its time comes, from the start
// and in a run that starts at a seek, and the seeked run keeps its picture
// on its audio's clock.
func TestIntegrationBurnedTextSubtitleRenders(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithSegmentTimeout(30*time.Second))
	info := genSubtitled(t, e)
	src := filepath.Join(e.dir, "subtitled.mkv")
	// libass draws nothing without a font (a host with no fontconfig
	// configuration or fonts): that is the host, not this path.
	probe, _ := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-v", "verbose", "-i", src, "-filter_complex",
		"[0:0]subtitles=filename="+escapeFilterValue(src)+":si=1[v]", "-map", "[v]", "-ss", "2", "-frames:v", "1", "-f", "null", "-").CombinedOutput()
	if bytes.Contains(probe, []byte("failed to find any fallback")) {
		t.Skip("libass finds no font on this host")
	}
	band := func(path string, at float64) []byte {
		t.Helper()
		// Decoded from the start (output -ss): seeking an MPEG-TS input
		// can land on a later keyframe.
		out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-v", "error", "-i", path, "-ss", strconv.FormatFloat(at, 'f', 3, 64),
			"-frames:v", "1", "-vf", "crop=640:100:0:260,format=gray", "-f", "rawvideo", "-").Output()
		if err != nil || len(out) != 640*100 {
			t.Fatalf("frame of %s at %.1f: %v (%d bytes)", filepath.Base(path), at, err, len(out))
		}
		return out
	}
	diff := func(a, b []byte) float64 {
		sum := 0
		for i := range a {
			sum += max(int(a[i])-int(b[i]), int(b[i])-int(a[i]))
		}
		return float64(sum) / float64(len(a))
	}
	caps := core.ClientCaps{VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, MaxHeight: 1080}
	for _, start := range []time.Duration{0, 12 * time.Second} {
		opts := allow()
		opts.DirectPlay, opts.SubtitleStream, opts.BurnSubtitles, opts.BurnTextSubtitles = false, 3, true, true
		d := e.m.Decide(*info, caps, opts)
		if !d.BurnSubtitles || d.SubtitleStream != 3 {
			t.Fatalf("decision does not burn the ASS track: %+v", d)
		}
		hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "burn-" + start.String(), InputURL: e.url("subtitled.mkv"), Info: *info, Decision: d, StartOffset: start})
		if err != nil {
			t.Fatal(err)
		}
		var joined []byte
		for n := 0; n < 4; n++ { // the session's segments count from its offset
			seg := fetch(t, hs, n)
			if n == 0 {
				streams := e.probeFile(t, e.writeTemp(t, "burn-first.ts", seg))
				if v, a := startTime(t, streams, "video"), startTime(t, streams, "audio"); math.Abs(v-a) > 0.2 {
					t.Fatalf("run at %v: video starts %.3f, audio %.3f", start, v, a)
				}
			}
			joined = append(joined, seg...)
		}
		_ = hs.Close()
		path := e.writeTemp(t, "burn-"+strconv.Itoa(int(start.Seconds()))+".ts", joined)
		cue, gap := 2.0, 6.0 // the cue at 1-4 s, nothing at 5-11 s
		if start > 0 {
			cue, gap = 13, 18
		}
		at := func(title float64) float64 { return title - start.Seconds() }
		if got := diff(band(path, at(cue)), band(src, cue)); got < 30 {
			t.Fatalf("run at %v: no cue burned at %.0f s (band differs by %.1f)", start, cue, got)
		}
		if got := diff(band(path, at(gap)), band(src, gap)); got > 12 {
			t.Fatalf("run at %v: a cue burned at %.0f s, where there is none (band differs by %.1f)", start, gap, got)
		}
	}
}
