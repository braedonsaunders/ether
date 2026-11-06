package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// Open-GOP HEVC in HLS fMP4. A CRA keyframe decodes before the leading
// pictures it presents after, so its composition offset is larger than the
// stream's reorder delay. hlsenc's mp4 muxer (dash mode) used to rewrite
// it to the previous fragment's end, presenting every CRA 0.125s early
// (x265 defaults at 24fps), and chunk slotting read the decode time, a
// segment early.

func TestHLSFMP4MuxSkipsSidx(t *testing.T) {
	s := runSpec{Decision: core.Decision{Container: ContainerFMP4, VideoCopy: true}, Dir: t.TempDir(), Playlist: "p.m3u8", InitName: "init.mp4"}
	if got := argValue(hlsMuxArgs(s), "-hls_segment_options"); got != "movflags=+frag_discont+skip_sidx" {
		t.Errorf("fMP4 segment options %q", got)
	}
	s.NormalizeNegativeDTS = true
	if got := argValue(hlsMuxArgs(s), "-hls_segment_options"); got != "movflags=+frag_discont+skip_sidx:avoid_negative_ts=make_non_negative" {
		t.Errorf("negative decode time retry options %q", got)
	}
	s.Decision.Container = ContainerMPEGTS
	if got := argValue(hlsMuxArgs(s), "-hls_segment_options"); got != "" {
		t.Errorf("TS segment options %q", got)
	}
	s.Decision.Container, s.Protocol = ContainerFMP4, core.ProtocolDASH
	if got := argValue(buildArgs(s), "-format_options"); got != "movflags=+frag_discont" {
		t.Errorf("DASH format options changed: %q", got)
	}
}

func TestTrunFirstOffset(t *testing.T) {
	sample := func(vals ...uint32) [][]byte {
		var out [][]byte
		for _, v := range vals {
			out = append(out, u32(v))
		}
		return out
	}
	neg := uint32(0xFFFFFAD0) // int32 -1328
	for _, tc := range []struct {
		name  string
		box   []byte
		cto   int64
		n     uint32
		valid bool
	}{
		// ffmpeg's video run: data offset, first sample flags, then
		// duration, size, composition offset per sample.
		{"v0 0xb05", mkFullBox("trun", 0, 0xb05, sample(2, 596, 0x02000000, 672, 9000, 3328, 672, 400, 1328)...), 3328, 2, true},
		{"v0 unsigned high bit", mkFullBox("trun", 0, 0x800, sample(1, neg)...), int64(neg), 1, true},
		{"v1 signed", mkFullBox("trun", 1, 0xf01, sample(1, 100, 672, 9000, 0x02000000, neg)...), -1328, 1, true},
		{"no offsets", mkFullBox("trun", 0, 0x301, sample(2, 100, 1024, 10, 1024, 11)...), 0, 2, true},
		{"empty", mkFullBox("trun", 0, 0x800, sample(0)...), 0, 0, true},
		{"truncated", mkFullBox("trun", 0, 0xb05, sample(1, 596, 0, 672, 9000)...), 0, 1, false},
		{"short", mkFullBox("trun", 0, 0)[:10], 0, 0, false},
	} {
		p := tc.box[8:]
		cto, n, ok := trunFirstOffset(p)
		if cto != tc.cto || n != tc.n || ok != tc.valid {
			t.Errorf("%s: cto=%d n=%d ok=%v, want %d %d %v", tc.name, cto, n, ok, tc.cto, tc.n, tc.valid)
		}
	}
}

func TestEditShift(t *testing.T) {
	entry0 := func(seg uint32, media int32) []byte {
		return append(append(u32(seg), u32(uint32(media))...), u32(0x10000)...)
	}
	entry1 := func(seg uint64, media int64) []byte {
		return append(append(u64(seg), u64(uint64(media))...), u32(0x10000)...)
	}
	for _, tc := range []struct {
		name string
		elst []byte
		want int64
		ok   bool
	}{
		{"ffmpeg v0", mkFullBox("elst", 0, 0, u32(1), entry0(0, 1328)), 1328, true},
		{"v1", mkFullBox("elst", 1, 0, u32(1), entry1(0, 3328)), 3328, true},
		// 0.5s empty edit (movie timescale 1000) delays media 0.083s in.
		{"empty edit first", mkFullBox("elst", 0, 0, u32(2), entry0(500, -1), entry0(0, 1328)), 1328 - 8000, true},
		{"v1 empty edit first", mkFullBox("elst", 1, 0, u32(2), entry1(500, -1), entry1(0, 1328)), 1328 - 8000, true},
		{"truncated", mkFullBox("elst", 0, 0, u32(2), entry0(500, -1)), 0, false},
		{"only empty edits", mkFullBox("elst", 0, 0, u32(1), entry0(500, -1)), 0, false},
		{"bad version", mkFullBox("elst", 2, 0, u32(1), entry0(0, 1328)), 0, false},
		{"short", mkFullBox("elst", 0, 0)[:10], 0, false},
	} {
		got, err := editShift(tc.elst[8:], 1000, 16000)
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("%s: %d %v, want %d ok=%v", tc.name, got, err, tc.want, tc.ok)
		}
	}
	if _, err := editShift(mkFullBox("elst", 0, 0, u32(2), entry0(500, -1), entry0(0, 1328))[8:], 0, 16000); err == nil {
		t.Error("empty edit without a movie timescale: no error")
	}
}

// openGOPInit is an HLS fMP4 init as ffmpeg writes it for open-GOP HEVC:
// video track 1 at 16000 with the first sample's composition offset as
// its edit, and audio track 2.
func openGOPInit(edit uint32) []byte { return openGOPInitScale(edit, 16000) }

// openGOPInitScale is openGOPInit with the video track at timescale scale.
func openGOPInitScale(edit, scale uint32) []byte {
	return openGOPInitBoxes(scale, mkFullBox("elst", 0, 0, u32(1), u32(0), u32(edit), u32(0x10000)), 1)
}

// openGOPInitBoxes builds it with the video edit list videoElst and
// videos video tracks.
func openGOPInitBoxes(scale uint32, videoElst []byte, videos int) []byte {
	ftyp := mkBox("ftyp", []byte("iso5"), u32(512), []byte("iso5iso6mp41"))
	mvhd := mkFullBox("mvhd", 0, 0, u32(0), u32(0), u32(1000), u32(0), make([]byte, 80))
	trak := func(id, scale uint32, handler string, elst []byte, entry []byte) []byte {
		tkhd := mkFullBox("tkhd", 0, 3, u32(0), u32(0), u32(id), u32(0), u32(0), make([]byte, 60))
		edts := mkBox("edts", elst)
		mdhd := mkFullBox("mdhd", 0, 0, u32(0), u32(0), u32(scale), u32(0), u16(0x55c4), u16(0))
		hdlr := mkFullBox("hdlr", 0, 0, u32(0), []byte(handler), make([]byte, 12), []byte("Handler\x00"))
		stbl := mkBox("stbl", mkFullBox("stsd", 0, 0, u32(1), entry))
		return mkBox("trak", tkhd, edts, mkBox("mdia", mdhd, hdlr, mkBox("minf", stbl)))
	}
	hvc1 := mkBox("hvc1", make([]byte, 6), u16(1), make([]byte, 16), u16(640), u16(360), make([]byte, 50))
	traks := [][]byte{mvhd, trak(2, 48000, "soun", mkFullBox("elst", 0, 0, u32(1), u32(0), u32(0), u32(0x10000)), audioEntry("mp4a", 2, 48000, esdsAAC(2)))}
	for i := 0; i < videos; i++ {
		traks = append(traks, trak(uint32(1+2*i), scale, "vide", videoElst, hvc1))
	}
	return append(ftyp, mkBox("moov", traks...)...)
}

// openGOPChunk is one HLS fMP4 fragment: audio first, then video whose
// first sample (the keyframe) has composition offset cto.
func openGOPChunk(videoTfdt uint64, version byte, cto uint32) []byte {
	return openGOPChunkTfdt(mkFullBox("tfdt", 1, 0, u64(videoTfdt)), version, cto)
}

// openGOPChunkTfdt is openGOPChunk with the video tfdt box given (nil none).
func openGOPChunkTfdt(tfdt []byte, version byte, cto uint32) []byte {
	audio := mkBox("traf", mkFullBox("tfhd", 0, 0x020000, u32(2)), mkFullBox("tfdt", 1, 0, u64(96000)),
		mkFullBox("trun", 0, 0x301, u32(1), u32(0), u32(1024), u32(10)))
	video := mkBox("traf", mkFullBox("tfhd", 0, 0x020038, u32(1), u32(672), u32(100), u32(0x01010000)), tfdt,
		mkFullBox("trun", version, 0xb05, u32(2), u32(0), u32(0x02000000), u32(672), u32(50), u32(cto), u32(672), u32(50), u32(cto-2000)))
	moof := mkBox("moof", mkFullBox("mfhd", 0, 0, u32(3)), audio, video)
	return bytes.Join([][]byte{mkBox("styp", []byte("msdh"), u32(0), []byte("msdhmsix")), moof, mkBox("mdat", make([]byte, 110))}, nil)
}

func TestFMP4ChunkStartIsKeyframePresentation(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The source keyframe presents at 2.000s and decodes at 1.792s; the
	// stream's reorder delay (the edit) is 0.083s. ffmpeg writes decode
	// time 1.792+0.083.
	for _, tc := range []struct {
		name    string
		edit    uint32
		tfdt    uint64
		version byte
		cto     uint32
		want    time.Duration
	}{
		{"open GOP", 1328, 30000, 0, 3328, 2 * time.Second},
		// A run restarted on that keyframe: its own edit is the keyframe's.
		{"restart on CRA", 3328, 32000, 0, 3328, 2 * time.Second},
		// negative_cts_offsets style: signed, no edit.
		{"signed offsets", 0, 33328, 1, uint32(0xFFFFFAD0), 2 * time.Second},
		// The muxer's old rewrite: the keyframe at the previous end.
		{"rewritten offset", 1328, 30000, 0, 1328, 1875 * time.Millisecond},
	} {
		cr := &chunkRun{container: ContainerFMP4, initPath: write(tc.name+"-init.mp4", openGOPInit(tc.edit))}
		got, err := cr.chunkStart(write(tc.name+".m4s", openGOPChunk(tc.tfdt, tc.version, tc.cto)))
		if err != nil || got != tc.want {
			t.Errorf("%s: start %v %v, want %v", tc.name, got, err, tc.want)
		}
		if cr.vTrack != 1 || cr.vScale != 16000 || cr.vShift != int64(tc.edit) {
			t.Errorf("%s: video track %d/%d/%d", tc.name, cr.vTrack, cr.vScale, cr.vShift)
		}
	}
	// A malformed run on the video track is an error, not decode time.
	bad := openGOPChunk(30000, 0, 3328)
	i := bytes.LastIndex(bad, []byte("trun"))
	bad[i+11] = 9 // sample count 9: the run is too short
	if _, err := fmp4ChunkStart(write("bad.m4s", bad), 1, 16000); err == nil {
		t.Error("truncated video trun: no error")
	}
	if _, err := fmp4ChunkStart(filepath.Join(dir, "bad.m4s"), 2, 48000); err != nil {
		t.Errorf("audio track beside a bad video run: %v", err)
	}
	negative := openGOPChunk(^uint64(1599), 0, 3328)
	if _, err := fmp4ChunkStart(write("negative.m4s", negative), 1, 16000); !errors.Is(err, errNegativeFMP4DecodeTime) {
		t.Errorf("wrapped negative decode time: %v", err)
	}
	if _, err := fmp4ChunkStart(write("missing.m4s", openGOPChunkTfdt(nil, 0, 3328)), 1, 16000); err == nil || errors.Is(err, errNegativeFMP4DecodeTime) {
		t.Errorf("missing decode time should not trigger normalization: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Real ffmpeg: open-GOP x265 copied to HLS fMP4.

type srcPacket struct {
	pts  float64
	size int
	key  bool
}

func (e *realEnv) packets(t *testing.T, file, sel string) []srcPacket {
	t.Helper()
	out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffprobe, "-v", "error", "-select_streams", sel, "-show_entries",
		"packet=pts_time,size,flags", "-of", "csv=p=0", file).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", file, err)
	}
	var pk []srcPacket
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		pts, err1 := strconv.ParseFloat(f[0], 64)
		size, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: packet %q", file, line)
		}
		pk = append(pk, srcPacket{pts: pts, size: size, key: strings.HasPrefix(f[2], "K")})
	}
	return pk
}

// matchSource requires out's packets to be a run of src's with the same
// presentation times (within tol). Leading pictures of out's first
// keyframe (presenting before it) are counted and skipped when skipLead.
func matchSource(t *testing.T, label string, src, out []srcPacket, tol float64, skipLead bool) (lead int) {
	t.Helper()
	if len(out) == 0 {
		t.Errorf("%s: no packets", label)
		return 0
	}
	at := -1
	for i := range src {
		ok := true
		for k := 0; k < 8 && k < len(out); k++ {
			if i+k >= len(src) || src[i+k].size != out[k].size {
				ok = false
				break
			}
		}
		if ok {
			at = i
			break
		}
	}
	if at < 0 {
		t.Errorf("%s: packets do not match the source", label)
		return 0
	}
	bad := 0
	for k, o := range out {
		if at+k >= len(src) || src[at+k].size != o.size || src[at+k].key != o.key {
			t.Errorf("%s: packet %d differs from source %d", label, k, at+k)
			return lead
		}
		s := src[at+k]
		if skipLead && !o.key && s.pts < out[0].pts && k < 8 {
			lead++
			continue
		}
		if math.Abs(o.pts-s.pts) > tol {
			if bad++; bad <= 3 {
				t.Errorf("%s: packet %d (key %v) presents at %.4f, source %.4f", label, k, o.key, o.pts, s.pts)
			}
		}
	}
	if bad > 3 {
		t.Errorf("%s: %d packets off the source", label, bad)
	}
	return lead
}

func TestIntegrationFMP4OpenGOPMatchesSource(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	// x265 defaults: open GOP (CRA with RASL leading pictures), 1s GOPs.
	e.gen(t, "opengop.mkv", 24, "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "log-level=error:keyint=24:min-keyint=24:scenecut=0:open-gop=1", "-c:a", "aac", "-ac", "2")
	mp4 := filepath.Join(e.dir, "opengop.mp4")
	if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", filepath.Join(e.dir, "opengop.mkv"),
		"-map", "0", "-c", "copy", "-tag:v", "hvc1", mp4).CombinedOutput(); err != nil {
		t.Fatalf("remux to mp4: %v %s", err, out)
	}
	for _, fx := range []struct {
		name string
		// Matroska keeps millisecond times: the muxer starts each
		// fragment at the previous decode time plus its millisecond
		// duration, up to 1ms off (as for closed GOPs). MP4 is exact.
		tol float64
	}{{"opengop.mkv", 0.0015}, {"opengop.mp4", 0.00005}} {
		t.Run(fx.name, func(t *testing.T) {
			src := filepath.Join(e.dir, fx.name)
			srcV, srcA := e.packets(t, src, "v"), e.packets(t, src, "a")
			lead := 0
			for i, p := range srcV {
				if i > 0 && i+1 < len(srcV) && p.key && srcV[i+1].pts < p.pts {
					lead++
				}
			}
			if lead < 3 {
				t.Fatalf("fixture has %d keyframes with leading pictures; want open GOPs", lead)
			}
			info, err := e.m.Probe(t.Context(), e.url(fx.name))
			if err != nil {
				t.Fatal(err)
			}
			info.Keyframes = e.keyframeIndex(t, fx.name)
			d := core.Decision{Kind: core.DecisionRemux, Container: ContainerFMP4, VideoCopy: true, AudioCopy: true, AudioStream: 1}
			hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "opengop-" + fx.name, InputURL: e.url(fx.name), Info: *info, Decision: d})
			if err != nil {
				t.Fatal(err)
			}
			defer hs.Close()
			s := hs.(*Session)
			if !s.plan.Keyframed() {
				t.Fatal("plan not built from the keyframe index")
			}
			starts := planStarts(t, hs, 0)
			total := len(starts) - 1
			init0 := fetch(t, hs, InitSegment)
			mkv := strings.HasSuffix(fx.name, ".mkv")
			e.checkOnClock(t, hs, init0, starts, 0, srcV, srcA, fx.tol, false, "first")
			mid := total / 2
			s.mu.Lock()
			for n := range s.done[0] {
				if n >= mid {
					delete(s.done[0], n)
				}
			}
			err = s.startRunLocked(mid, false)
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			// The restart starts on a CRA: its own edit is the CRA's
			// offset, larger than init0's. Under init0 (and the init now
			// served) it must present exactly as the source.
			e.checkOnClock(t, hs, init0, starts, mid, srcV, srcA, fx.tol, mkv, "restart")
			e.checkOnClock(t, hs, fetch(t, hs, InitSegment), starts, mid, srcV, srcA, fx.tol, mkv, "restart-refetched-init")
		})
	}
}

// checkOnClock fetches segments from..end of hs and requires each, under
// init, to start on its plan keyframe at the source's time, and the
// joined segments to present every source packet (video and audio) at its
// source time within tol. With skipLead (Matroska after a seek) the
// leading pictures of the first keyframe are counted, not checked: ffmpeg
// guesses Matroska decode times after a seek and collapses theirs (an
// input-side defect outside the muxer; a fresh decoder drops them).
func (e *realEnv) checkOnClock(t *testing.T, hs core.HLSSession, init []byte, starts []float64, from int,
	srcV, srcA []srcPacket, tol float64, skipLead bool, label string) {
	t.Helper()
	joined := append([]byte(nil), init...)
	for n := from; n < len(starts)-1; n++ {
		seg := fetch(t, hs, n)
		joined = append(joined, seg...)
		file := e.writeTemp(t, fmt.Sprintf("%s-%d.mp4", label, n), append(append([]byte(nil), init...), seg...))
		v := e.packets(t, file, "v")
		if len(v) == 0 || !v[0].key || math.Abs(v[0].pts-starts[n]) > tol {
			t.Errorf("%s segment %d starts %+v, plan %.4f", label, n, firstPacket(v), starts[n])
		}
		if n == from {
			e.decodesAlone(t, ContainerFMP4, init, seg, fmt.Sprintf("%s-%d", label, n))
		}
	}
	file := e.writeTemp(t, label+"-joined.mp4", joined)
	lead := matchSource(t, label+" video", srcV, e.packets(t, file, "v"), tol, skipLead)
	if lead > 4 {
		t.Errorf("%s: %d leading pictures skipped", label, lead)
	}
	if skipLead {
		t.Logf("%s: skipped %d leading pictures of the first keyframe", label, lead)
	}
	matchSource(t, label+" audio", srcA, e.packets(t, file, "a"), tol, false)
}

// A failover to a release with a deeper reorder delay (x265 bframes=4
// with a B-pyramid against bframes=2): the replacement's edit is larger
// at every keyframe, so its decode times must move onto the first run's
// clock for players keeping init0.
func TestIntegrationFMP4FailoverChangedDelay(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	for _, fx := range []struct{ name, params string }{
		{"shallow", "bframes=2:b-pyramid=0"},
		{"deep", "bframes=4:b-pyramid=1"},
	} {
		e.gen(t, fx.name+".mkv", 24, "-c:v", "libx265", "-preset", "ultrafast",
			"-x265-params", "log-level=error:keyint=24:min-keyint=24:scenecut=0:open-gop=1:repeat-headers=1:"+fx.params, "-c:a", "aac", "-ac", "2")
		if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", filepath.Join(e.dir, fx.name+".mkv"),
			"-map", "0", "-c", "copy", "-tag:v", "hvc1", filepath.Join(e.dir, fx.name+".mp4")).CombinedOutput(); err != nil {
			t.Fatalf("remux to mp4: %v %s", err, out)
		}
	}
	for _, c := range []struct {
		ext string
		tol float64
	}{{".mp4", 0.00005}, {".mkv", 0.0015}} {
		t.Run(c.ext, func(t *testing.T) {
			from, to := "shallow"+c.ext, "deep"+c.ext
			info, err := e.m.Probe(t.Context(), e.url(from))
			if err != nil {
				t.Fatal(err)
			}
			info.Keyframes = e.keyframeIndex(t, from)
			d := core.Decision{Kind: core.DecisionRemux, Container: ContainerFMP4, VideoCopy: true, AudioCopy: true, AudioStream: 1}
			hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "delay" + c.ext, InputURL: e.url(from), Info: *info, Decision: d})
			if err != nil {
				t.Fatal(err)
			}
			defer hs.Close()
			starts := planStarts(t, hs, 0)
			init0 := fetch(t, hs, InitSegment)
			srcV, srcA := e.packets(t, filepath.Join(e.dir, from), "v"), e.packets(t, filepath.Join(e.dir, from), "a")
			e.checkOnClock(t, hs, init0, starts, 0, srcV, srcA, c.tol, false, "before")

			toInfo, err := e.m.Probe(t.Context(), e.url(to))
			if err != nil {
				t.Fatal(err)
			}
			toInfo.Keyframes = e.keyframeIndex(t, to)
			mid := (len(starts) - 1) / 2
			if err := hs.SwapInput(context.Background(), e.url(to), *toInfo, time.Duration(starts[mid]*float64(time.Second))); err != nil {
				t.Fatal(err)
			}
			dstV, dstA := e.packets(t, filepath.Join(e.dir, to), "v"), e.packets(t, filepath.Join(e.dir, to), "a")
			mkv := c.ext == ".mkv"
			e.checkOnClock(t, hs, init0, starts, mid, dstV, dstA, c.tol, mkv, "failover")
			e.checkOnClock(t, hs, fetch(t, hs, InitSegment), starts, mid, dstV, dstA, c.tol, mkv, "failover-refetched-init")
			// The replacement's own edit is deeper: the test is vacuous
			// unless the correction had something to do.
			s := hs.(*Session)
			s.mu.Lock()
			cur := s.cur
			s.mu.Unlock()
			if cur == nil || cur.edits == nil || len(cur.edits.delta) == 0 {
				t.Errorf("failover run needed no correction: %+v", cur.edits)
			} else {
				t.Logf("failover run moved decode times by %v (own edits %v)", cur.edits.delta, cur.edits.own)
			}
		})
	}
}

func firstPacket(p []srcPacket) srcPacket {
	if len(p) == 0 {
		return srcPacket{}
	}
	return p[0]
}

// A failover inside one init scope to a release whose output video
// counts in another timescale: every fragment time it writes means
// something else under init0, so its output must never be served.
func TestIntegrationFMP4FailoverChangedTimescaleRefused(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	e.gen(t, "ts16k.mkv", 24, "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "log-level=error:keyint=24:min-keyint=24:scenecut=0:open-gop=1:repeat-headers=1", "-c:a", "aac", "-ac", "2")
	for _, fx := range []struct{ name, scale string }{{"ts16k.mp4", "16000"}, {"ts90k.mp4", "90000"}} {
		if out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", filepath.Join(e.dir, "ts16k.mkv"),
			"-map", "0", "-c", "copy", "-tag:v", "hvc1", "-video_track_timescale", fx.scale, filepath.Join(e.dir, fx.name)).CombinedOutput(); err != nil {
			t.Fatalf("remux: %v %s", err, out)
		}
	}
	info, err := e.m.Probe(t.Context(), e.url("ts16k.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	info.Keyframes = e.keyframeIndex(t, "ts16k.mp4")
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerFMP4, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "timescale", InputURL: e.url("ts16k.mp4"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	s := hs.(*Session)
	starts := planStarts(t, hs, 0)
	init0 := fetch(t, hs, InitSegment)
	srcV, srcA := e.packets(t, filepath.Join(e.dir, "ts16k.mp4"), "v"), e.packets(t, filepath.Join(e.dir, "ts16k.mp4"), "a")
	e.checkOnClock(t, hs, init0, starts, 0, srcV, srcA, 0.00005, false, "before")
	tracks, err := parseInitTracks(init0)
	if err != nil {
		t.Fatal(err)
	}

	toInfo, err := e.m.Probe(t.Context(), e.url("ts90k.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	toInfo.Keyframes = e.keyframeIndex(t, "ts90k.mp4")
	mid := (len(starts) - 1) / 2
	if err := hs.SwapInput(context.Background(), e.url("ts90k.mp4"), *toInfo, time.Duration(starts[mid]*float64(time.Second))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	hs.ServeSegment(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), mid)
	if rec.Code == http.StatusOK {
		t.Errorf("segment %d of the 90000 timescale run served: %d bytes", mid, rec.Body.Len())
	}
	s.mu.Lock()
	for n := mid; n < len(starts)-1; n++ {
		if s.done[0][n] {
			t.Errorf("segment %d of the refused run published", n)
		}
	}
	initRel := s.initFileLocked(0)
	s.mu.Unlock()
	served, err := os.ReadFile(filepath.Join(s.dir, initRel))
	if err != nil || !bytes.Equal(served, init0) {
		t.Errorf("served init changed from init0 (%v)", err)
	}
	// Not vacuous: the refused run's own init counts video in 90000.
	refused := false
	_ = filepath.WalkDir(s.dir, func(p string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || filepath.Ext(p) != ".mp4" {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil {
			if ts, err := parseInitTracks(b); err == nil {
				for _, tr := range ts {
					if tr.Handler == "vide" && tr.Timescale == 90000 {
						refused = true
					}
				}
			}
		}
		return nil
	})
	if !refused || tracks[0].Timescale == 90000 {
		t.Errorf("replacement init at 90000 not found (init0 tracks %+v)", tracks)
	}
	if rec.Code != http.StatusOK {
		t.Logf("segment %d after the refused failover: HTTP %d %s", mid, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
