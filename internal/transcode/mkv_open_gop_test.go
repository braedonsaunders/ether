package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// openGOPx265 encodes an HDR10 HEVC fixture with x265's open GOPs: every
// keyframe after the first is a CRA followed in decode order by leading
// pictures that display before it, as in most HEVC releases.
func openGOPx265(keyint int, params string) []string {
	k := strconv.Itoa(keyint)
	return []string{"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc",
		"-x265-params", "pools=1:frame-threads=1:open-gop=1:bframes=3:scenecut=0:keyint=" + k + ":min-keyint=" + k + ":" + params,
		"-c:a", "flac"}
}

// videoPacket is one video packet of a Matroska file, in decode order.
type videoPacket struct {
	pts, dts float64
	key      bool
}

func (e *realEnv) videoPackets(t *testing.T, file string) []videoPacket {
	t.Helper()
	out, err := mediaproc.CommandContext(t.Context(), 30*time.Second, e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,dts_time,flags", "-of", "csv=p=0", file).Output()
	if err != nil {
		t.Fatalf("ffprobe packets %s: %v", file, err)
	}
	var pkts []videoPacket
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		pts, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		dts, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			dts = pts
		}
		pkts = append(pkts, videoPacket{pts: pts, dts: dts, key: strings.HasPrefix(f[2], "K")})
	}
	return pkts
}

// keyframe is source's keyframe at at (media seconds): how far before it
// its leading pictures display (0 for a closed GOP), and its reorder delay
// (presentation less decode time).
func (e *realEnv) keyframe(t *testing.T, source string, at float64) (lead, reorder float64) {
	t.Helper()
	pkts := e.videoPackets(t, filepath.Join(e.dir, source))
	for i, p := range pkts {
		if !p.key || math.Abs(p.pts-at) > 0.001 {
			continue
		}
		for _, q := range pkts[i+1:] {
			if q.key {
				break
			}
			lead = math.Max(lead, at-q.pts)
		}
		return lead, p.pts - p.dts
	}
	t.Fatalf("%s has no keyframe at %.3f", source, at)
	return 0, 0
}

// gopFrames is the output times of the video frames of source's GOPs whose
// keyframes are in [from, to) media seconds, without the leading pictures
// of the keyframe at from when a decoder discards them there (it starts
// at that keyframe, or the keyframe was spliced).
func (e *realEnv) gopFrames(t *testing.T, source string, from, to float64, dropLead bool) []float64 {
	t.Helper()
	var out []float64
	key := math.Inf(-1)
	for _, p := range e.videoPackets(t, filepath.Join(e.dir, source)) {
		if p.key {
			key = p.pts
		}
		if key < from-0.001 || key >= to-0.001 || (dropLead && math.Abs(key-from) < 0.001 && p.pts < from-0.001) {
			continue
		}
		out = append(out, p.pts+matroskaOrigin.Seconds())
	}
	sort.Float64s(out)
	return out
}

// checkOpenGOPSegment requires a published segment to hold exactly the
// GOPs whose keyframes fall in its playlist span: its first packet the
// keyframe at the span's start (or no media at all: an alias inside a GOP
// that started earlier), and nothing at or after the next segment's start
// but leading pictures of a later keyframe never arrive without it.
func (e *realEnv) checkOpenGOPSegment(t *testing.T, header, seg []byte, s *Session, n int, wantKey bool, label string) {
	t.Helper()
	origin := matroskaOrigin.Seconds()
	start, end := origin+s.segStart(n).Seconds(), origin+s.segStart(n+1).Seconds()
	if !wantKey {
		if len(seg) > emptyClusterSize {
			t.Errorf("%s segment %d: %d bytes, want an empty Cluster inside the earlier GOP", label, n, len(seg))
		}
		return
	}
	pkts := e.videoPackets(t, e.writeTemp(t, fmt.Sprintf("%s-seg%d.mkv", label, n), append(append([]byte(nil), header...), seg...)))
	if len(pkts) == 0 {
		t.Fatalf("%s segment %d holds no video", label, n)
	}
	if !pkts[0].key || math.Abs(pkts[0].pts-start) > 0.002 {
		t.Errorf("%s segment %d starts with packet %.3f (key %v), playlist says keyframe %.3f", label, n, pkts[0].pts, pkts[0].key, start)
	}
	for _, p := range pkts {
		if p.key && p.pts >= end-0.002 && n+1 < s.total {
			t.Errorf("%s segment %d holds keyframe %.3f of segment %d (from %.3f)", label, n, p.pts, n+1, end)
			break
		}
	}
}

// checkJoined plays a joined stream the way Plex's Matroska player does:
// one decoder over the header and every Cluster. It must decode without
// error and present exactly the video frames want (output seconds), in
// order. Audio must cover the video and advance a frame at a time, except
// where a restarted run's audio encoder starts (output seconds; 0 none):
// its input is cut at the seek point, while the audio before it went with
// the keyframe Cluster the player did not get, so it may resume up to the
// keyframe's reorder delay plus a frame late. That audio is also the most
// a joined stream that stops at a segment boundary ends short of its
// video: the next Cluster holds it.
func (e *realEnv) checkJoined(t *testing.T, joined []byte, label string, want []float64, restartAt, reorder float64) {
	t.Helper()
	checkClusterCRCs(t, joined, label)
	path := e.writeTemp(t, label+"-joined.mkv", joined)
	if out, err := mediaproc.CommandContext(t.Context(), 30*time.Second, e.ffmpeg, "-v", "error", "-xerror", "-err_detect", "explode", "-i", path, "-map", "0", "-f", "null", "-").CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) > 0 {
		t.Fatalf("%s: joined stream does not decode: %v\n%s", label, err, out)
	}
	video := e.frameTimes(t, path, "v:0")
	for i := 0; i < max(len(video), len(want)); i++ {
		if i >= len(video) || i >= len(want) || math.Abs(video[i]-want[i]) > 0.002 {
			got, exp := math.NaN(), math.NaN()
			if i < len(video) {
				got = video[i]
			}
			if i < len(want) {
				exp = want[i]
			}
			t.Fatalf("%s: video frame %d presents at %.3f, want %.3f (%d frames, want %d)", label, i, got, exp, len(video), len(want))
		}
	}
	audio := e.frameTimes(t, path, "a:0")
	const aac = 1024.0 / 48000
	if len(audio) == 0 || audio[0] > video[0]+aac || audio[len(audio)-1] < video[len(video)-1]-reorder-2*aac {
		t.Fatalf("%s: audio %.3f-%.3f does not cover the video %.3f-%.3f", label, audio[0], audio[len(audio)-1], video[0], video[len(video)-1])
	}
	for i := 1; i < len(audio); i++ {
		step := audio[i] - audio[i-1]
		limit := 1.5 * aac
		if restartAt > 0 && audio[i] > restartAt-2*aac && audio[i] < restartAt+aac {
			// The encoder's first frame (its priming shifts it a frame
			// early).
			limit = reorder + 1.5*aac
		}
		if step <= 0 || step > limit {
			t.Fatalf("%s: audio %.3f -> %.3f, at most %.3f apart", label, audio[i-1], audio[i], limit)
		}
	}
}

// checkClusterCRCs verifies the CRC-32 every Cluster of a joined stream
// carries (the header's elements are ffmpeg's own).
func checkClusterCRCs(t *testing.T, joined []byte, label string) {
	t.Helper()
	r := bytes.NewReader(joined)
	end := int64(len(joined))
	checked := 0
	for off := int64(0); off < end; {
		id, body, size, err := ebmlElement(r, off, end)
		if err != nil {
			t.Fatalf("%s: element at %d: %v", label, off, err)
		}
		if size < 0 || id == mkvSegment {
			off = body // descend into the Segment
			continue
		}
		if id == mkvCluster {
			cid, cb, cs, err := ebmlElement(r, body, body+size)
			if err == nil && cid == mkvCRC32 && cs == 4 {
				if crc32.ChecksumIEEE(joined[cb+4:body+size]) != binary.LittleEndian.Uint32(joined[cb:]) {
					t.Fatalf("%s: Cluster at %d fails its CRC-32", label, off)
				}
				checked++
			}
		}
		off = body + size
	}
	if checked == 0 {
		t.Fatalf("%s: no Cluster CRC-32 to check", label)
	}
}

// TestIntegrationMatroskaOpenGOPCopyTimeline plays open-GOP HEVC copied
// into segmented Matroska with the one header the player fetched at
// startup, across release swaps, a same-release restart and seeks. Every
// Cluster's keyframe must fall on its playlist segment (not a GOP early or
// late), and the joined stream must present exactly the frames of the
// GOPs delivered: no overlap, nothing backwards, no gap but the leading
// pictures a decoder must drop at a splice or a seek.
func TestIntegrationMatroskaOpenGOPCopyTimeline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keyint int // original keyframe interval, frames at 24 fps
		swapAt time.Duration
		other  []string // the replacement release's encode
	}{
		// GOPs shorter than a segment: every segment starts a GOP.
		{"short-gop", 24, 8 * time.Second, openGOPx265(24, "ctu=32:no-sao=1:cbqpoffs=3")},
		// Another release encoded with identical parameter sets: only the
		// pictures differ, so they cannot tell the streams apart.
		{"short-gop-same-params", 24, 8 * time.Second, append([]string{"-vf", "hflip"}, openGOPx265(24, "ctu=64")...)},
		// 8s GOPs over 4s segments: every other segment is an empty
		// Cluster inside the GOP before it.
		{"long-gop", 192, 8 * time.Second, openGOPx265(24, "ctu=32:no-sao=1:cbqpoffs=3")},
		// A swap inside a GOP the player already holds to its end.
		{"long-gop-mid", 192, 12 * time.Second, openGOPx265(24, "ctu=32:no-sao=1:cbqpoffs=3")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRealEnv(t, WithoutHWDetection())
			e.gen(t, "remux.mkv", 24, openGOPx265(tc.keyint, "ctu=64")...)
			e.gen(t, "web.mkv", 24, tc.other...)
			gop := float64(tc.keyint) / 24
			if lead, _ := e.keyframe(t, "remux.mkv", gop); lead <= 0 {
				t.Fatal("original has closed GOPs; the fixture needs leading pictures")
			}
			if lead, _ := e.keyframe(t, "web.mkv", 8); lead <= 0 {
				t.Fatal("replacement has closed GOPs; the fixture needs leading pictures")
			}
			sameSets := bytes.Equal(e.extradata(t, "remux.mkv"), e.extradata(t, "web.mkv"))
			if sameSets != strings.HasSuffix(tc.name, "same-params") {
				t.Fatalf("releases share parameter sets: %v", sameSets)
			}
			hasKey := func(n int, gop time.Duration) bool { return (time.Duration(n)*4*time.Second)%gop == 0 }
			start := func(id string) (*Session, []byte) {
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
				hs, err := e.m.StartHLS(ctx, core.HLSRequest{SessionID: id, InputURL: e.url("remux.mkv"), Info: *info, Decision: d})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = hs.Close() })
				s := hs.(*Session)
				if s.segDur != 4*time.Second || s.total != 6 {
					t.Fatalf("plan: %v x %d", s.segDur, s.total)
				}
				return s, fetch(t, hs, InitSegment)
			}
			origGOP := time.Duration(tc.keyint) * time.Second / 24
			// swap plays segments in order and fails over to release at
			// tc.swapAt, as Plex's player does: one decoder throughout.
			swap := func(t *testing.T, release string, gop time.Duration) (joined []byte, resume time.Duration, slices []int) {
				s, header := start("open-gop-" + strings.TrimSuffix(release, ".mkv") + "-" + tc.name)
				joined = append([]byte(nil), header...)
				swapSeg := s.plan.IndexAt(tc.swapAt)
				for n := 0; n < swapSeg; n++ {
					seg := fetch(t, s, n)
					e.checkOpenGOPSegment(t, header, seg, s, n, hasKey(n, origGOP), "original")
					joined = append(joined, seg...)
				}
				info, err := e.m.Probe(context.Background(), e.url(release))
				if err != nil {
					t.Fatal(err)
				}
				if err := s.SwapInput(context.Background(), e.url(release), *info, tc.swapAt); err != nil {
					t.Fatal(err)
				}
				if !s.Decision().VideoCopy {
					t.Fatal("replacement is not copied")
				}
				resume = tc.swapAt
				if r := time.Duration(swapSeg) * 4 * time.Second % origGOP; r != 0 {
					// The player already holds the original's GOP to its
					// end: the replacement continues from there.
					resume += origGOP - r
				}
				for n := swapSeg; n < s.total; n++ {
					seg := fetch(t, s, n)
					at := time.Duration(n) * 4 * time.Second
					e.checkOpenGOPSegment(t, header, seg, s, n, at >= resume && at%gop == 0, "replacement")
					if slices == nil && len(seg) > emptyClusterSize {
						slices = e.keySliceTypes(t, header, seg)
					}
					joined = append(joined, seg...)
				}
				return joined, resume, slices
			}

			t.Run("swap", func(t *testing.T) {
				joined, resume, slices := swap(t, "web.mkv", time.Second)
				// Every slice of the replacement's first keyframe.
				if !allTypes(slices, hevcBLAWLP) {
					t.Errorf("replacement's first keyframe slices are types %v, want BLA_W_LP", slices)
				}
				// The replacement's first keyframe is spliced: its
				// leading pictures need the replacement's GOP before it.
				at := resume.Seconds()
				want := append(e.gopFrames(t, "remux.mkv", 0, at, false), e.gopFrames(t, "web.mkv", at, 24, true)...)
				_, reorder := e.keyframe(t, "remux.mkv", at)
				e.checkJoined(t, joined, "swap", want, at+matroskaOrigin.Seconds(), reorder)
			})

			t.Run("same-release", func(t *testing.T) {
				// A failover to the same release (or any restart of its
				// copy) continues one stream: the leading pictures of the
				// restart's keyframe decode against the GOP before it.
				joined, resume, slices := swap(t, "remux.mkv", origGOP)
				if !allTypes(slices, hevcCRA) {
					t.Errorf("restart's first keyframe slices are types %v, want CRA", slices)
				}
				at := resume.Seconds()
				_, reorder := e.keyframe(t, "remux.mkv", at)
				e.checkJoined(t, joined, "same-release", e.gopFrames(t, "remux.mkv", 0, 24, false), at+matroskaOrigin.Seconds(), reorder)
			})

			t.Run("seek", func(t *testing.T) {
				s, header := start("open-gop-seek-" + tc.name)
				seg0 := fetch(t, s, 0)
				e.checkOpenGOPSegment(t, header, seg0, s, 0, true, "start")
				// Seek ahead before the first run got past segment 0, as
				// serveSegment restarts for a request far ahead of it.
				s.mu.Lock()
				for n := range s.done[0] {
					if n > 0 {
						delete(s.done[0], n)
					}
				}
				err := s.startRunLocked(4, false)
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				// A fresh decoder from segment 4's keyframe.
				ahead := fetch(t, s, 4)
				e.checkOpenGOPSegment(t, header, ahead, s, 4, true, "seek")
				e.decodesAlone(t, ContainerMatroska, header, ahead, "seek-4")
				seg5 := fetch(t, s, 5)
				e.checkOpenGOPSegment(t, header, seg5, s, 5, hasKey(5, origGOP), "seek")
				_, reorder := e.keyframe(t, "remux.mkv", gop)
				e.checkJoined(t, bytes.Join([][]byte{header, ahead, seg5}, nil), "seek-ahead", e.gopFrames(t, "remux.mkv", 16, 24, true), 0, reorder)
				// Seek back to segment 1, behind the run: another restart,
				// and a fresh decoder, since the player last had segment 5.
				// It continues from there in order.
				joined := append([]byte(nil), header...)
				first := 0.0
				for n := 1; n < 4; n++ {
					seg := fetch(t, s, n)
					if n == 1 {
						e.decodesAlone(t, ContainerMatroska, header, seg, "seek-back-1")
						pkts := e.videoPackets(t, e.writeTemp(t, "seek-back-1.mkv", append(append([]byte(nil), header...), seg...)))
						first = pkts[0].pts - matroskaOrigin.Seconds()
					}
					joined = append(joined, seg...)
					s.mu.Lock()
					published := s.trackSegPath(0, n)
					s.mu.Unlock()
					b, err := os.ReadFile(published)
					if err != nil {
						t.Fatal(err)
					}
					e.checkOpenGOPSegment(t, header, b, s, n, hasKey(n, origGOP), "seek-back")
				}
				// Segment 1 is served from its GOP's keyframe (inside an
				// 8s GOP: the one at 0s).
				if want := math.Floor(4/gop) * gop; math.Abs(first-want) > 0.002 {
					t.Errorf("seek to segment 1 starts at %.3f, want the keyframe at %.3f", first, want)
				}
				e.checkJoined(t, joined, "seek-back", e.gopFrames(t, "remux.mkv", first, 16, true), 0, reorder)
			})
		})
	}
}

// keySliceTypes is the NAL unit types of the slices of a segment's first
// video keyframe, read with the header's track and NAL unit length size.
func (e *realEnv) keySliceTypes(t *testing.T, header, seg []byte) []int {
	t.Helper()
	video, nalLen, err := mkvVideoTrack(e.writeTemp(t, "slices-header.mkv", header))
	if err != nil || video == 0 || nalLen != 4 {
		t.Fatalf("header: video track %d, NAL length size %d: %v", video, nalLen, err)
	}
	path := e.writeTemp(t, "slices-seg.mkv", seg)
	raw := seg
	var types []int
	if err := mkvWalk(path, nil, func(b mkvFrame) bool {
		if b.track != video || !b.key {
			return true
		}
		frame := raw[b.data : b.data+b.size]
		nals, err := nalHeaders(frame, nalLen)
		if err != nil {
			t.Fatalf("keyframe NAL units: %v", err)
		}
		for _, at := range nals {
			if typ := int(frame[at]>>1) & 0x3F; typ < 32 {
				types = append(types, typ)
			}
		}
		return false
	}); err != nil {
		t.Fatal(err)
	}
	return types
}

func allTypes(types []int, want int) bool {
	for _, t := range types {
		if t != want {
			return false
		}
	}
	return len(types) > 0
}

// extradata is the video CodecPrivate (parameter sets) of a fixture.
func (e *realEnv) extradata(t *testing.T, source string) []byte {
	t.Helper()
	out, err := mediaproc.CommandContext(t.Context(), 30*time.Second, e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_data_hash", "SHA256", "-show_entries", "stream=extradata_hash", "-of", "csv=p=0", filepath.Join(e.dir, source)).Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Fatalf("extradata of %s: %v", source, err)
	}
	return bytes.TrimSpace(out)
}

// mkvTestCluster is a sized Cluster with a CRC-32, as matroskaenc writes:
// its Timestamp (ms) and SimpleBlocks.
func mkvTestCluster(ms uint16, blocks ...[]byte) []byte {
	body := append([]byte{0xE7, 0x82, byte(ms >> 8), byte(ms)}, bytes.Join(blocks, nil)...)
	sum := make([]byte, 4)
	binary.LittleEndian.PutUint32(sum, crc32.ChecksumIEEE(body))
	body = append(append([]byte{0xBF, 0x84}, sum...), body...)
	return append([]byte{0x1F, 0x43, 0xB6, 0x75, 0x10, 0x00, byte(len(body) >> 8), byte(len(body))}, body...)
}

// mkvTestBlock is a SimpleBlock of track at rel ms.
func mkvTestBlock(track byte, rel int16, key bool, frame []byte) []byte {
	flags := byte(0)
	if key {
		flags = 0x80
	}
	b := append([]byte{0x80 | track, byte(uint16(rel) >> 8), byte(rel), flags}, frame...)
	return append([]byte{0xA3, 0x40 | byte(len(b)>>8), byte(len(b))}, b...)
}

// hevcNAL is an HEVC NAL unit (nuh_layer_id 0, TemporalId 0) with a
// length prefix of nalLen bytes, or an Annex B start code (nalLen 0).
func hevcNAL(nalLen int, typ byte, payload ...byte) []byte {
	nal := append([]byte{typ << 1, 1}, payload...)
	if nalLen == 0 {
		return append([]byte{0, 0, 0, 1}, nal...)
	}
	prefix := make([]byte, nalLen)
	for i, n := nalLen-1, len(nal); i >= 0; i, n = i-1, n>>8 {
		prefix[i] = byte(n)
	}
	return append(prefix, nal...)
}

func TestMatroskaSpliceHEVC(t *testing.T) {
	dir := t.TempDir()
	au := func(nalLen int, pic byte) []byte {
		// In-band parameter sets, then two slices of one picture.
		return bytes.Join([][]byte{hevcNAL(nalLen, 32, 0xAA), hevcNAL(nalLen, 33, 1, 1), hevcNAL(nalLen, 34, 0xCC),
			hevcNAL(nalLen, pic, 0x80, 0x11), hevcNAL(nalLen, pic, 0x00, 0x22)}, nil)
	}
	audio := mkvTestBlock(1, -208, true, []byte{0xFF, 0xF1})
	types := func(path string, nalLen int) []int {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		if err := mkvWalk(path, nil, func(b mkvFrame) bool {
			if b.track != 2 || !b.key {
				return true
			}
			frame := raw[b.data : b.data+b.size]
			nals, err := nalHeaders(frame, nalLen)
			if err != nil {
				t.Fatalf("NAL units of %s: %v", path, err)
			}
			for _, at := range nals {
				got = append(got, int(frame[at]>>1)&0x3F)
			}
			return false
		}); err != nil {
			t.Fatalf("walk %s: %v", path, err)
		}
		return got
	}
	for _, tc := range []struct {
		name    string
		nalLen  int
		pic     byte
		spliced bool
	}{
		{"open GOP, hvcC lengths", 4, hevcCRA, true},
		{"open GOP, 2-byte lengths", 2, hevcCRA, true},
		{"open GOP, Annex B", 0, hevcCRA, true},
		{"closed GOP", 4, 19, false}, // IDR_W_RADL
	} {
		chunk := filepath.Join(dir, "chunk.mkv")
		// The audio held back for the keyframe, the keyframe, and a
		// leading picture.
		before := mkvTestCluster(18000, audio, mkvTestBlock(2, 0, true, au(tc.nalLen, tc.pic)), mkvTestBlock(2, -125, false, hevcNAL(tc.nalLen, 8, 0x80)))
		if err := os.WriteFile(chunk, before, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := nalHeaders(au(tc.nalLen, tc.pic), tc.nalLen); err != nil || len(got) != 5 {
			t.Fatalf("%s: %d NAL units, %v", tc.name, len(got), err)
		}
		spliced, err := mkvSpliceHEVC(chunk, 2, tc.nalLen)
		if err != nil || spliced != tc.spliced {
			t.Fatalf("%s: spliced=%v err=%v", tc.name, spliced, err)
		}
		want := []int{32, 33, 34, int(tc.pic), int(tc.pic)}
		if tc.spliced {
			want[3], want[4] = hevcBLAWLP, hevcBLAWLP
		}
		if got := types(chunk, tc.nalLen); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: NAL types %v, want %v", tc.name, got, want)
		}
		after, err := os.ReadFile(chunk)
		if err != nil {
			t.Fatal(err)
		}
		changed := 0
		for i := range before {
			if before[i] != after[i] {
				changed++
			}
		}
		switch {
		case len(after) != len(before):
			t.Errorf("%s: %d bytes, was %d", tc.name, len(after), len(before))
		case !tc.spliced && changed != 0:
			t.Errorf("%s: %d bytes changed", tc.name, changed)
		case tc.spliced && (changed < 3 || changed > 6):
			t.Errorf("%s: %d bytes changed, want the two NAL headers and the CRC", tc.name, changed)
		}
		// The CRC-32 still covers the Cluster after it.
		body := after[8:]
		if crc32.ChecksumIEEE(body[6:]) != binary.LittleEndian.Uint32(body[2:6]) {
			t.Errorf("%s: Cluster CRC-32 does not match", tc.name)
		}
		if start, err := mkvChunkStart(chunk, 2); err != nil || start != 8*time.Second {
			t.Errorf("%s: chunk start %v %v", tc.name, start, err)
		}
	}
}

// A keyframe the splice cannot account for completely is left alone.
func TestMatroskaSpliceHEVCRejects(t *testing.T) {
	dir := t.TempDir()
	cra := bytes.Join([][]byte{hevcNAL(4, 33, 1, 1), hevcNAL(4, hevcCRA, 0x80, 0x11), hevcNAL(4, hevcCRA, 0x00, 0x22)}, nil)
	trail := hevcNAL(4, 1, 0x00, 0x33) // TRAIL_R
	for _, tc := range []struct {
		name   string
		frame  []byte
		nalLen int
	}{
		{"truncated NAL unit", cra[:len(cra)-1], 4},
		{"trailing length bytes", append(append([]byte(nil), cra...), 0, 0), 4},
		{"NAL unit shorter than its header", append(append([]byte(nil), cra...), 0, 0, 0, 1, 0x2A), 4},
		{"CRA and non-CRA slices", append(append([]byte(nil), cra...), trail...), 4},
		{"wrong length size", cra, 2},
		{"unknown framing", cra, -1},
		{"length-prefixed read as Annex B", cra, 0},
	} {
		chunk := filepath.Join(dir, "chunk.mkv")
		before := mkvTestCluster(18000, mkvTestBlock(2, 0, true, tc.frame))
		if err := os.WriteFile(chunk, before, 0o644); err != nil {
			t.Fatal(err)
		}
		if spliced, err := mkvSpliceHEVC(chunk, 2, tc.nalLen); err == nil || spliced {
			t.Errorf("%s: spliced=%v err=%v, want an error", tc.name, spliced, err)
		}
		if after, _ := os.ReadFile(chunk); !bytes.Equal(after, before) {
			t.Errorf("%s: chunk changed", tc.name)
		}
	}
}

// spliceRunner writes segmented Matroska runs like ffmpeg's stream segment
// muxer: a header with an HEVC (hvcC) video track, then one chunk per 1s
// GOP from the keyframe at -ss, each Cluster starting with the audio held
// back for its keyframe. GOPs after the first are open (CRA); a run of an
// input in bad writes keyframes whose NAL units are truncated. Runs keep
// going until stopped, like ffmpeg mid-title.
type spliceRunner struct {
	*fakeRunner
	bad map[string]bool
}

type blockedProc struct {
	done chan struct{}
	once sync.Once
}

func (p *blockedProc) Done() <-chan struct{} { return p.done }
func (p *blockedProc) Err() error            { <-p.done; return errors.New("signal: killed") }
func (p *blockedProc) Stop()                 { p.once.Do(func() { close(p.done) }) }
func (p *blockedProc) Pause() error          { return nil }
func (p *blockedProc) Resume() error         { return nil }

func (r spliceRunner) Start(cmd Command) (Process, error) {
	args := cmd.Args
	if argValue(args, "-segment_format") != "matroska" {
		return r.fakeRunner.Start(cmd)
	}
	el := func(id []byte, body ...[]byte) []byte {
		b := bytes.Join(body, nil)
		return append(append(append([]byte(nil), id...), byte(0x40|len(b)>>8), byte(len(b))), b...)
	}
	hvcC := make([]byte, 23)
	hvcC[0], hvcC[21] = 1, 0xF3
	header := bytes.Join([][]byte{
		el([]byte{0x1A, 0x45, 0xDF, 0xA3}, el([]byte{0x42, 0x82}, []byte("matroska"))),
		{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		el([]byte{0x16, 0x54, 0xAE, 0x6B},
			el([]byte{0xAE}, el([]byte{0xD7}, []byte{1}), el([]byte{0x83}, []byte{1}), el([]byte{0x86}, []byte("V_MPEGH/ISO/HEVC")), el([]byte{0x63, 0xA2}, hvcC)),
			el([]byte{0xAE}, el([]byte{0xD7}, []byte{2}), el([]byte{0x83}, []byte{2}))),
	}, nil)
	if err := os.WriteFile(argValue(args, "-segment_header_filename"), header, 0o644); err != nil {
		return nil, err
	}
	ss, _ := strconv.ParseFloat(argValue(args, "-ss"), 64)
	bad := r.bad[argValue(args, "-i")]
	var list strings.Builder
	list.WriteString("#EXTM3U\n")
	for n, k := 0, int(ss); k < 24; n, k = n+1, k+1 {
		pic := byte(hevcCRA)
		if k == 0 {
			pic = 19 // IDR_W_RADL
		}
		key := bytes.Join([][]byte{hevcNAL(4, 32, 0xAA), hevcNAL(4, 33, 1, 1), hevcNAL(4, 34, 0xCC), hevcNAL(4, pic, 0x80, byte(k))}, nil)
		if bad {
			key = key[:len(key)-1]
		}
		chunk := mkvTestCluster(uint16((k+10)*1000), mkvTestBlock(2, -208, true, []byte{0xFF, 0xF1}),
			mkvTestBlock(1, 0, true, key), mkvTestBlock(1, -125, false, hevcNAL(4, 8, 0x80)))
		name := fmt.Sprintf(args[len(args)-1], n)
		if err := os.WriteFile(name, chunk, 0o644); err != nil {
			return nil, err
		}
		fmt.Fprintf(&list, "#EXTINF:1.000000,\n%s\n", filepath.Base(name))
	}
	if err := os.WriteFile(argValue(args, "-segment_list"), []byte(list.String()), 0o644); err != nil {
		return nil, err
	}
	return &blockedProc{done: make(chan struct{})}, nil
}

// A keyframe that must be spliced and cannot be fails its run through the
// usual failure handling; nothing of it is published. A release that can
// be spliced then continues the stream with a BLA keyframe.
func TestMatroskaUnspliceableRunFails(t *testing.T) {
	const a, b, c = "http://127.0.0.1:1/a", "http://127.0.0.1:1/b", "http://127.0.0.1:1/c"
	failed := make(chan error, 4)
	sr := spliceRunner{fakeRunner: newFakeRunner(), bad: map[string]bool{b: true}}
	m := newTestManager(t, sr.fakeRunner, config.Transcode{}, WithRunner(sr),
		WithInputFailureHandler(func(_, input string, _ time.Duration, err error) {
			if input == b {
				failed <- err
			}
		}))
	info := mustParse(t, "mkv_hevc_hdr10.json")
	info.Duration = 24 * time.Second
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	hs, err := m.StartHLS(t.Context(), core.HLSRequest{SessionID: "unspliceable", InputURL: a, Info: info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	s := hs.(*Session)
	header := fetch(t, s, InitSegment)
	for n := 0; n < 2; n++ {
		_ = fetch(t, s, n)
	}
	if err := s.SwapInput(t.Context(), b, info, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if !strings.Contains(err.Error(), "splice") {
			t.Errorf("input failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the unspliceable run did not fail")
	}
	s.mu.Lock()
	published := s.done[0][2]
	path := s.trackSegPath(0, 2)
	s.mu.Unlock()
	if _, err := os.Stat(path); published || err == nil {
		t.Fatalf("segment 2 published from the unspliceable run (done=%v)", published)
	}
	if rec := get(t, s, 2); rec.Code == http.StatusOK {
		t.Fatal("segment 2 served from the unspliceable run")
	}

	if err := s.SwapInput(t.Context(), c, info, 8*time.Second); err != nil {
		t.Fatal(err)
	}
	seg := fetch(t, s, 2)
	dir := t.TempDir()
	hp, sp := filepath.Join(dir, "h.mkv"), filepath.Join(dir, "s.mkv")
	if os.WriteFile(hp, header, 0o644) != nil || os.WriteFile(sp, seg, 0o644) != nil {
		t.Fatal("write")
	}
	video, nalLen, err := mkvVideoTrack(hp)
	if err != nil || video != 1 || nalLen != 4 {
		t.Fatalf("header: %d %d %v", video, nalLen, err)
	}
	var types []int
	if err := mkvWalk(sp, nil, func(f mkvFrame) bool {
		if f.track != video || !f.key {
			return true
		}
		frame := seg[f.data : f.data+f.size]
		nals, err := nalHeaders(frame, nalLen)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range nals {
			if typ := int(frame[at]>>1) & 0x3F; typ < 32 {
				types = append(types, typ)
			}
		}
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if !allTypes(types, hevcBLAWLP) {
		t.Errorf("replacement keyframe slices %v, want BLA_W_LP", types)
	}
	checkClusterCRCs(t, seg, "spliced segment")
}
