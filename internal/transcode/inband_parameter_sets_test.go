package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// A WEB-DL remux of a DASH avc3/hev1 stream often carries an avcC/hvcC
// record without parameter sets: they live only in-band. The MP4 copy is
// tagged avc1/hvc1, which promises a complete record, so the record must be
// rebuilt from the in-band sets or AVPlayer fetches the init and the first
// segment and stops (seen with BBC iPlayer UHD HLG releases).

func TestInBandParameterSetsRecords(t *testing.T) {
	sps, pps := []byte{0x67, 0x64, 0x00, 0x1f, 0xac}, []byte{0x68, 0xee, 0x3c, 0x80}
	avcc := func(nsps, npps int) []byte {
		b := []byte{1, 0x64, 0x00, 0x1f, 0xff, 0xe0 | byte(nsps)}
		for range nsps {
			b = binary.BigEndian.AppendUint16(b, uint16(len(sps)))
			b = append(b, sps...)
		}
		b = append(b, byte(npps))
		for range npps {
			b = binary.BigEndian.AppendUint16(b, uint16(len(pps)))
			b = append(b, pps...)
		}
		return b
	}
	hvcc := func(arrays ...byte) []byte {
		b := make([]byte, 23)
		b[0], b[1], b[12], b[22] = 1, 0x02, 153, byte(len(arrays))
		for _, typ := range arrays {
			b = append(b, typ, 0, 1, 0, 2, typ<<1, 1)
		}
		return b
	}
	for _, tc := range []struct {
		name   string
		codec  string
		record []byte
		want   bool
	}{
		{"avcC complete", "h264", avcc(1, 1), false},
		{"avcC without SPS/PPS", "h264", avcc(0, 0), true},
		{"avcC without PPS", "h264", avcc(1, 0), true},
		{"avcC truncated", "h264", avcc(1, 1)[:9], true},
		{"hvcC complete", "hevc", hvcc(32, 33, 34), false},
		{"hvcC without arrays", "hevc", hvcc(), true},
		{"hvcC without PPS", "hevc", hvcc(32, 33), true},
		{"Annex B", "hevc", []byte{0, 0, 0, 1, 0x40, 0x01}, false},
		{"empty", "h264", nil, false},
		{"other codec", "av1", avcc(0, 0), false},
	} {
		if got := inBandParameterSets(tc.codec, tc.record); got != tc.want {
			t.Errorf("%s: inBandParameterSets = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMP4CopyRebuildsInBandParameterSets(t *testing.T) {
	info := func(codec string, inBand bool) core.MediaInfo {
		return core.MediaInfo{Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: codec, Width: 3840, Height: 2160, InBandParameterSets: inBand, HEVCParameterSets: codec == "hevc" && !inBand},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000},
		}}
	}
	for _, tc := range []struct {
		name      string
		info      core.MediaInfo
		container string
		strip     bool
		want      string
	}{
		{"hevc fmp4", info("hevc", true), ContainerFMP4, false, "hevc_metadata"},
		{"hevc fmp4 dv strip", info("hevc", true), ContainerFMP4, true, "dovi_rpu=strip=1,hevc_metadata"},
		{"h264 fmp4", info("h264", true), ContainerFMP4, false, "h264_metadata"},
		{"hevc complete", info("hevc", false), ContainerFMP4, false, ""},
		{"h264 complete", info("h264", false), ContainerFMP4, false, ""},
		{"hevc mpegts", info("hevc", true), ContainerMPEGTS, false, ""},
		{"h264 matroska", info("h264", true), ContainerMatroska, false, "h264_mp4toannexb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := runSpec{Info: tc.info, Input: "http://127.0.0.1/in", Decision: core.Decision{
				Container: tc.container, VideoCopy: true, AudioCopy: true, AudioStream: 1, StripDolbyVision: tc.strip}}
			if got := argValue(buildCodecArgs(s), "-bsf:v"); got != tc.want {
				t.Errorf("-bsf:v = %q, want %q", got, tc.want)
			}
		})
	}
	for _, codec := range []string{"h264", "hevc"} {
		i := info(codec, true)
		if got, want := argValue(downloadArgs("http://127.0.0.1/in", i.Video(), &i.Streams[1], "/tmp/out.mp4"), "-bsf:v"), codec+"_metadata"; got != want {
			t.Errorf("%s download -bsf:v = %q, want %q", codec, got, want)
		}
		i = info(codec, false)
		if got := argValue(downloadArgs("http://127.0.0.1/in", i.Video(), &i.Streams[1], "/tmp/out.mp4"), "-bsf:v"); got != "" {
			t.Errorf("%s complete download -bsf:v = %q", codec, got)
		}
	}
}

// stripParameterSets rewrites a Matroska file's video CodecPrivate to a
// record without parameter sets (hvcC: the 23-byte header with no arrays;
// avcC: the 7-byte header with no SPS or PPS), padding the freed bytes with
// an EBML Void element so every other offset stays valid.
func stripParameterSets(t *testing.T, codec, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	keep := 23
	if codec == "h264" {
		keep = 7
	}
	for off := 0; ; {
		j := bytes.Index(b[off:], []byte{0x63, 0xa2})
		if j < 0 {
			t.Fatal("no video CodecPrivate found")
		}
		i := off + j
		off = i + 1
		var n, hl int
		switch {
		case i+3 < len(b) && b[i+2]&0x80 != 0:
			n, hl = int(b[i+2]&0x7f), 3
		case i+4 < len(b) && b[i+2]&0xc0 == 0x40:
			n, hl = int(b[i+2]&0x3f)<<8|int(b[i+3]), 4
		default:
			continue
		}
		body := i + hl
		if n < keep+4 || body+n > len(b) || b[body] != 1 || inBandParameterSets(codec, b[body:body+n]) {
			continue
		}
		if codec == "h264" {
			b[body+5], b[body+6] = 0xe0, 0
		} else {
			b[body+22] = 0
		}
		if hl == 3 {
			b[i+2] = 0x80 | byte(keep)
		} else {
			b[i+2], b[i+3] = 0x40, byte(keep)
		}
		rest := b[body+keep : body+n]
		if len(rest)-2 <= 126 {
			rest[0], rest[1] = 0xec, 0x80|byte(len(rest)-2)
			clear(rest[2:])
		} else {
			rest[0], rest[1], rest[2] = 0xec, 0x40|byte((len(rest)-3)>>8), byte(len(rest)-3)
			clear(rest[3:])
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
}

// recordParameterSets counts the parameter sets in the first avcC/hvcC of
// an MP4 header and requires the matching avc1/hvc1 sample entry.
func recordParameterSets(t *testing.T, codec string, mp4 []byte) int {
	t.Helper()
	entry, box := "hvc1", "hvcC"
	if codec == "h264" {
		entry, box = "avc1", "avcC"
	}
	if !bytes.Contains(mp4, []byte(entry)) {
		t.Fatalf("no %s sample entry", entry)
	}
	i := bytes.Index(mp4, []byte(box))
	if i < 0 || i+4+23 > len(mp4) {
		t.Fatalf("no %s", box)
	}
	c := mp4[i+4:]
	if codec == "h264" {
		return int(c[5] & 0x1f)
	}
	return int(c[22])
}

func TestIntegrationMP4CopyRebuildsInBandParameterSets(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection())
	for _, tc := range []struct {
		codec string
		gen   []string
	}{
		{"hevc", []string{"-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-preset", "ultrafast",
			"-x265-params", "log-level=error:repeat-headers=1:keyint=24:colorprim=bt2020:transfer=arib-std-b67:colormatrix=bt2020nc"}},
		{"h264", []string{"-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-x264-params", "repeat-headers=1:keyint=24"}},
	} {
		t.Run(tc.codec, func(t *testing.T) {
			src, name := "inband-"+tc.codec+"-src.mkv", "inband-"+tc.codec+".mkv"
			e.gen(t, src, 6, append(tc.gen, "-c:a", "aac", "-ac", "2")...)
			stripParameterSets(t, tc.codec, filepath.Join(e.dir, src), filepath.Join(e.dir, name))
			info, err := e.m.Probe(t.Context(), e.url(name))
			if err != nil {
				t.Fatal(err)
			}
			if v := info.Video(); v == nil || !v.InBandParameterSets {
				t.Fatalf("fixture not probed as in-band parameter sets: %+v", v)
			}

			d := core.Decision{Kind: core.DecisionRemux, Container: ContainerFMP4, VideoCopy: true, AudioCopy: true, AudioStream: 1}
			hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "inband-" + tc.codec, InputURL: e.url(name), Info: *info, Decision: d})
			if err != nil {
				t.Fatal(err)
			}
			defer hs.Close()
			init := fetch(t, hs, InitSegment)
			if n := recordParameterSets(t, tc.codec, init); n == 0 {
				t.Fatal("HLS init record has no parameter sets")
			}
			e.decodesAlone(t, ContainerFMP4, init, fetch(t, hs, 0), "inband-"+tc.codec)

			out := filepath.Join(e.dir, "inband-"+tc.codec+".mp4")
			args := downloadArgs(e.url(name), info.Video(), &info.Streams[1], out)
			if b, err := mediaproc.CommandContext(t.Context(), time.Minute, e.ffmpeg, args...).CombinedOutput(); err != nil {
				t.Fatalf("download remux: %v %s", err, b)
			}
			mp4, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if n := recordParameterSets(t, tc.codec, mp4); n == 0 {
				t.Fatal("download MP4 record has no parameter sets")
			}
			if strings.Contains(string(mp4[:min(len(mp4), 4096)]), "hev1") {
				t.Error("download carries an hev1 sample entry")
			}
		})
	}
}
