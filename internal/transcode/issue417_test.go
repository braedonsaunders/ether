package transcode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Issue #417: LG webOS negotiates HLS fMP4 with VideoCopy=true, AudioCopy=false
// (2s remuxOpenSegment). When a source has multi-segment GOPs (e.g. 8s HEVC
// GOPs spanning 00000..00003), sequential fetches inside the GOP must receive
// empty 56-byte fMP4 fragments rather than repeating segment 0's backward
// tfdt=0 (which locked webOS's GStreamer qtdemux in a 00001->00000->00003
// resync loop), while out-of-order seeks, Range retries, and post-init fetches
// receive the covering keyframe GOP.
func TestIssue417FMP4LongGOPSequentialAndSeek(t *testing.T) {
	fr := newFakeRunner()
	fr.chunkGOP = 8 * time.Second
	fr.chunkEnd = 24 * time.Second
	m := newTestManager(t, fr, config.Transcode{FastStart: true}, WithBuffering(60*time.Second, 0, 0))
	info := mustParse(t, "mp4_h264_aac.json")
	info.Duration = 24 * time.Second
	d := core.Decision{
		Kind:        core.DecisionTranscode,
		Container:   ContainerFMP4,
		VideoCopy:   true,
		AudioCopy:   false,
		AudioCodec:  "aac",
		AudioStream: 1,
	}
	hs, err := m.StartHLS(t.Context(), core.HLSRequest{
		SessionID: "issue417-lg-webos-gop",
		InputURL:  "http://127.0.0.1:1/coco.mkv",
		Info:      info,
		Decision:  d,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := hs.(*Session)
	if s.segDur != 2*time.Second || s.total != 12 {
		t.Fatalf("segDur=%v total=%d, want 2s and 12", s.segDur, s.total)
	}
	if r := get(t, s, InitSegment); r.Code != http.StatusOK {
		t.Fatalf("init: %d", r.Code)
	}

	lastTFDT := int64(-1)
	for n := 0; n < s.total; n++ {
		r := get(t, s, n)
		if r.Code != http.StatusOK {
			t.Fatalf("segment %d: HTTP %d (%s)", n, r.Code, r.Body.String())
		}
		b := r.Body.Bytes()
		if n%4 != 0 {
			if !isEmptyFMP4(b) {
				t.Fatalf("sequential alias segment %d is %d bytes, want an emptyFMP4 fragment", n, len(b))
			}
			boxes, err := readBoxes(b, 0, len(b))
			if err != nil || len(boxes) != 3 || boxes[0].typ != "styp" || boxes[1].typ != "moof" || boxes[2].typ != "mdat" {
				t.Fatalf("segment %d invalid emptyFMP4 boxes: %+v err=%v", n, boxes, err)
			}
			continue
		}
		wantMarker := fmt.Sprintf("t=%.3f", float64(n*2))
		if !bytes.Contains(b, []byte(wantMarker)) {
			t.Fatalf("segment %d missing GOP marker %s: %q", n, wantMarker, b)
		}
		tmp := filepath.Join(t.TempDir(), fmt.Sprintf("%05d.m4s", n))
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			t.Fatal(err)
		}
		at, err := fmp4ChunkStart(tmp, 1, 90000)
		if err != nil {
			t.Fatalf("segment %d fmp4ChunkStart: %v", n, err)
		}
		if int64(at) <= lastTFDT {
			t.Fatalf("segment %d decode time %v went backwards after %v", n, at, time.Duration(lastTFDT))
		}
		lastTFDT = int64(at)
	}

	// Out-of-order seek to alias 00003.m4s ([6s, 8s)) must serve the covering GOP at 0s.
	seek3 := get(t, s, 3)
	if seek3.Code != http.StatusOK || !bytes.Contains(seek3.Body.Bytes(), []byte("t=0.000")) {
		t.Fatalf("seek to segment 3: HTTP %d len=%d", seek3.Code, seek3.Body.Len())
	}
	if retry3 := get(t, s, 3); !bytes.Equal(retry3.Body.Bytes(), seek3.Body.Bytes()) {
		t.Fatalf("retry of seek segment 3 changed bytes: %d -> %d", seek3.Body.Len(), retry3.Body.Len())
	}
	req := httptest.NewRequest(http.MethodGet, "/00003.m4s", nil)
	req.Header.Set("Range", "bytes=16-")
	rec := httptest.NewRecorder()
	s.ServeSegment(rec, req, 3)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), seek3.Body.Bytes()[16:]) {
		t.Fatalf("Range retry on segment 3: status=%d len=%d", rec.Code, rec.Body.Len())
	}

	// Continuing sequentially from 3 -> 4 -> 5 serves GOP at 8s then empty alias at 5.
	if seg4 := get(t, s, 4); seg4.Code != http.StatusOK || !bytes.Contains(seg4.Body.Bytes(), []byte("t=8.000")) {
		t.Fatalf("segment 4 after seek: HTTP %d len=%d", seg4.Code, seg4.Body.Len())
	}
	if seg5 := get(t, s, 5); seg5.Code != http.StatusOK || !isEmptyFMP4(seg5.Body.Bytes()) {
		t.Fatalf("sequential segment 5: HTTP %d len=%d, want an emptyFMP4 fragment", seg5.Code, seg5.Body.Len())
	}

	// Fetching init.mp4 resets decoder continuity so the next segment (even 6 after 5) gets its covering GOP.
	if r := get(t, s, InitSegment); r.Code != http.StatusOK {
		t.Fatalf("re-fetch init: %d", r.Code)
	}
	if seg6 := get(t, s, 6); seg6.Code != http.StatusOK || !bytes.Contains(seg6.Body.Bytes(), []byte("t=8.000")) {
		t.Fatalf("segment 6 after fresh init: HTTP %d len=%d, want covering GOP at 8s", seg6.Code, seg6.Body.Len())
	}
}

// Issue #417: evicted segments in an open session must NEVER return 410 Gone.
// Both completely evicted segments and in-GOP alias placeholders whose covering
// GOP segment was evicted by keepBehind must regenerate on demand (200 OK with
// decodable keyframe bytes) while keeping disk usage bounded; 410 Gone is
// reserved strictly for stopped/closed sessions without reviving ffmpeg (#410).
func TestIssue417EvictedSegmentRegeneratesAndStoppedReturns410(t *testing.T) {
	fr := newFakeRunner()
	fr.chunkGOP = 8 * time.Second
	fr.chunkEnd = 40 * time.Second
	// restartAhead = 12s (6 segments), maxAhead = 16s (8 segments), keepBehind = 6s (3 segments of 2s).
	m := newTestManager(t, fr, config.Transcode{FastStart: true}, WithBuffering(12*time.Second, 16*time.Second, 6*time.Second))
	info := mustParse(t, "mp4_h264_aac.json")
	info.Duration = 40 * time.Second
	d := core.Decision{
		Kind:        core.DecisionTranscode,
		Container:   ContainerFMP4,
		VideoCopy:   true,
		AudioCopy:   false,
		AudioCodec:  "aac",
		AudioStream: 1,
	}
	hs, err := m.StartHLS(t.Context(), core.HLSRequest{
		SessionID: "issue417-eviction",
		InputURL:  "http://127.0.0.1:1/source.mkv",
		Info:      info,
		Decision:  d,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := hs.(*Session)
	if r := get(t, s, InitSegment); r.Code != http.StatusOK {
		t.Fatalf("init: %d", r.Code)
	}

	// Play sequentially 0..6: at segment 6 with behind=3, segments 0, 1, 2 are
	// evicted, leaving alias 3 on disk without its covering GOP 0.
	for n := 0; n <= 6; n++ {
		if r := get(t, s, n); r.Code != http.StatusOK {
			t.Fatalf("segment %d: HTTP %d", n, r.Code)
		}
	}
	s.mu.Lock()
	done0, done3 := s.done[0][0], s.done[0][3]
	s.mu.Unlock()
	if done0 {
		t.Fatal("segment 0 should have been evicted by keepBehind=6s at playhead 6")
	}
	if !done3 {
		t.Fatal("segment 3 should still be cached as an alias at playhead 6")
	}

	// Seeking to alias 3 whose covering GOP 0 was evicted must regenerate
	// keyframe media (200 OK), never serve a bare 56B placeholder or 410.
	regen3 := get(t, s, 3)
	if regen3.Code != http.StatusOK || isEmptyFMP4(regen3.Body.Bytes()) || !bytes.Contains(regen3.Body.Bytes(), []byte("t=0.000")) {
		t.Fatalf("evicted-GOP alias 3: HTTP %d len=%d, want regenerated GOP at 0s", regen3.Code, regen3.Body.Len())
	}

	// Advance to segment 14 so early segments (0..10) are fully evicted and
	// verify disk segment count stays bounded.
	for n := 7; n <= 14; n++ {
		if r := get(t, s, n); r.Code != http.StatusOK {
			t.Fatalf("segment %d: HTTP %d", n, r.Code)
		}
	}
	segFiles, _ := filepath.Glob(filepath.Join(s.dir, "*.m4s"))
	if len(segFiles) == 0 || len(segFiles) > 12 {
		t.Fatalf("expected bounded segment files on disk (1..12), found %d", len(segFiles))
	}
	for _, gone := range []int{0, 2} {
		if _, err := os.Stat(s.trackSegPath(0, gone)); !os.IsNotExist(err) {
			t.Fatalf("segment %d still on disk after advancing to 14: %v", gone, err)
		}
	}

	// Requesting completely evicted mid-GOP segment 2 and segment 0 in a live
	// session must each regenerate via ffmpeg and return 200 OK with keyframe bytes.
	for _, target := range []int{2, 0} {
		res := get(t, s, target)
		if res.Code != http.StatusOK {
			t.Fatalf("evicted segment %d returned HTTP %d (%s), want 200 OK", target, res.Code, res.Body.String())
		}
		if isEmptyFMP4(res.Body.Bytes()) || !bytes.Contains(res.Body.Bytes(), []byte("t=0.000")) {
			t.Fatalf("evicted segment %d len=%d, want regenerated keyframe GOP", target, res.Body.Len())
		}
	}

	// Once the session stops and closes (#410), playlist, init, and segment
	// requests return 410 Gone and never spawn another ffmpeg process.
	s.PlaybackState("stopped")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	startsBefore := fr.startCount()

	plRec := httptest.NewRecorder()
	s.ServePlaylist(plRec, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil))
	if plRec.Code != http.StatusGone {
		t.Fatalf("closed playlist: HTTP %d, want 410 Gone", plRec.Code)
	}
	if r := get(t, s, InitSegment); r.Code != http.StatusGone {
		t.Fatalf("closed init: HTTP %d, want 410 Gone", r.Code)
	}
	if r := get(t, s, 3); r.Code != http.StatusGone {
		t.Fatalf("closed segment 3: HTTP %d, want 410 Gone", r.Code)
	}
	if got := fr.startCount(); got != startsBefore {
		t.Fatalf("closed session spawned ffmpeg: starts %d -> %d", startsBefore, got)
	}
}

// Issue #417: verifies the two chunked-fMP4 resume/tail edge cases:
//  1. When a seek's video-only preroll chunk writes init-seek.mp4 without an elst
//     on soun while the retained init has a non-zero soun edit shift, the run
//     retains the original init and shifts subsequent audio fragments onto its
//     clock instead of failing with "track 2 has no edit list to move".
//  2. When a seek opens on an audio-only chunk (no video traf in moof) or ends
//     with trailing audio past video's #EXTINF, chunkStart falls back to the
//     audio track instead of wedging cr.next until segmentPadLimit, and
//     fmp4ChunkEnd extends the final chunk's span across the trailing slots.
func TestIssue417FMP4SeekPrerollAndAudioOnlyChunks(t *testing.T) {
	makeDualInit := func(videoShift int32, audioElst bool, audioShift int32) []byte {
		ftyp := mkBox("ftyp", []byte("iso5"), u32(512), []byte("iso5iso6mp41"))
		mvhd := mkFullBox("mvhd", 0, 0, u32(0), u32(0), u32(1000), u32(0), make([]byte, 80))
		trak := func(id, scale uint32, handler string, edts []byte, entry []byte) []byte {
			tkhd := mkFullBox("tkhd", 0, 3, u32(0), u32(0), u32(id), u32(0), u32(0), make([]byte, 60))
			mdhd := mkFullBox("mdhd", 0, 0, u32(0), u32(0), u32(scale), u32(0), u16(0x55c4), u16(0))
			hdlr := mkFullBox("hdlr", 0, 0, u32(0), []byte(handler), make([]byte, 12), []byte("Handler\x00"))
			stbl := mkBox("stbl", mkFullBox("stsd", 0, 0, u32(1), entry))
			if edts != nil {
				return mkBox("trak", tkhd, edts, mkBox("mdia", mdhd, hdlr, mkBox("minf", stbl)))
			}
			return mkBox("trak", tkhd, mkBox("mdia", mdhd, hdlr, mkBox("minf", stbl)))
		}
		hvc1 := mkBox("hvc1", make([]byte, 6), u16(1), make([]byte, 16), u16(640), u16(360), make([]byte, 50))
		vEdts := mkBox("edts", mkFullBox("elst", 0, 0, u32(1), u32(0), u32(uint32(videoShift)), u32(0x10000)))
		var aEdts []byte
		if audioElst {
			aEdts = mkBox("edts", mkFullBox("elst", 0, 0, u32(1), u32(0), u32(uint32(audioShift)), u32(0x10000)))
		}
		return append(ftyp, mkBox("moov",
			mvhd,
			trak(1, 16000, "vide", vEdts, hvc1),
			trak(2, 48000, "soun", aEdts, audioEntry("mp4a", 2, 48000, esdsAAC(2))),
		)...)
	}

	makeFrag := func(seq uint32, hasVideo bool, vTfdt uint64, vCto uint32, vSamples int, hasAudio bool, aTfdt uint64, aSamples int) []byte {
		kids := [][]byte{mkFullBox("mfhd", 0, 0, u32(seq))}
		if hasVideo {
			var trunArgs [][]byte
			trunArgs = append(trunArgs, u32(uint32(vSamples)), u32(0), u32(0x02000000))
			for i := 0; i < vSamples; i++ {
				cto := uint32(0)
				if i == 0 {
					cto = vCto
				}
				trunArgs = append(trunArgs, u32(672), u32(60), u32(cto))
			}
			vTraf := mkBox("traf",
				mkFullBox("tfhd", 0, 0x020038, u32(1), u32(672), u32(60), u32(0x01010000)),
				mkFullBox("tfdt", 1, 0, u64(vTfdt)),
				mkFullBox("trun", 0, 0xb05, trunArgs...),
			)
			kids = append(kids, vTraf)
		}
		if hasAudio {
			var trunArgs [][]byte
			trunArgs = append(trunArgs, u32(uint32(aSamples)), u32(0))
			for i := 0; i < aSamples; i++ {
				trunArgs = append(trunArgs, u32(1024), u32(32))
			}
			aTraf := mkBox("traf",
				mkFullBox("tfhd", 0, 0x020008, u32(2), u32(1024)),
				mkFullBox("tfdt", 1, 0, u64(aTfdt)),
				mkFullBox("trun", 0, 0x301, trunArgs...),
			)
			kids = append(kids, aTraf)
		}
		return bytes.Join([][]byte{
			mkBox("styp", []byte("msdh"), u32(0), []byte("msdhmsix")),
			mkBox("moof", kids...),
			mkBox("mdat", make([]byte, 128)),
		}, nil)
	}

	readTrackTFDT := func(t *testing.T, path string, wantID uint32) uint64 {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		top, err := readBoxes(b, 0, len(b))
		if err != nil {
			t.Fatal(err)
		}
		for _, bx := range top {
			if bx.typ != "moof" {
				continue
			}
			trafs, _ := readBoxes(b, bx.start+bx.hdr, bx.end)
			for _, traf := range trafs {
				if traf.typ != "traf" {
					continue
				}
				tfhd, ok := child(b, traf, "tfhd")
				if !ok || binary.BigEndian.Uint32(tfhd.payload(b)[4:8]) != wantID {
					continue
				}
				tfdt, ok := child(b, traf, "tfdt")
				if !ok {
					t.Fatalf("missing tfdt for track %d", wantID)
				}
				return binary.BigEndian.Uint64(tfdt.payload(b)[4:12])
			}
		}
		t.Fatalf("track %d not found in %s", wantID, path)
		return 0
	}

	stopInitialRun := func(s *Session) {
		s.mu.Lock()
		var proc Process
		if s.cur != nil {
			proc = s.cur.proc
		}
		s.stopRunLocked()
		s.mu.Unlock()
		if proc != nil {
			proc.Stop()
		}
	}

	t.Run("video-only preroll omits audio elst", func(t *testing.T) {
		fr := newFakeRunner()
		m := newTestManager(t, fr, config.Transcode{FastStart: true}, withPollInterval(time.Hour))
		info := mustParse(t, "mp4_h264_aac.json")
		info.Duration = 20 * time.Second
		d := core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCopy: true, AudioCopy: false, AudioCodec: "aac", AudioStream: 1}
		hs, err := m.StartHLS(t.Context(), core.HLSRequest{SessionID: "issue417-preroll", InputURL: "http://127.0.0.1:1/src.mkv", Info: info, Decision: d})
		if err != nil {
			t.Fatal(err)
		}
		s := hs.(*Session)
		stopInitialRun(s)

		s.mu.Lock()
		// Publish retained init-retained.mp4 with vide shift=1328 and soun shift=1024.
		initRet := filepath.Join(s.dir, "init-retained.mp4")
		if err := os.WriteFile(initRet, makeDualInit(1328, true, 1024), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		r1 := &run{seq: 1, startSeg: 0, spec: runSpec{Dir: s.dir, InitName: "init-retained.mp4", Decision: d}}
		if err := s.noteInitLocked(r1, 0); err != nil {
			s.mu.Unlock()
			t.Fatalf("noteInitLocked r1: %v", err)
		}

		// Simulate seek run r2 at segment 3 (6s): chunk 0 is video-only preroll
		// [4s, 6s), so init-seek.mp4 has vide shift=3328 (CRA) and NO soun elst;
		// chunk 1 at [6s, 8s) has both video and audio.
		cdir := filepath.Join(s.dir, "c-seek")
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.dir, "init-seek.mp4"), makeDualInit(3328, false, 0), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		// Video at 4.000s under own shift 3328: tfdt + cto - 3328 = 64000 => tfdt = 64000.
		if err := os.WriteFile(filepath.Join(cdir, "00000.m4s"), makeFrag(1, true, 64000, 3328, 48, false, 0, 0), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		// Video at 6.000s (96000 ticks) + audio at 6.000s (288000 ticks unshifted).
		if err := os.WriteFile(filepath.Join(cdir, "00001.m4s"), makeFrag(2, true, 96000, 3328, 48, true, 288000, 94), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		pl := "#EXTM3U\n#EXTINF:2.000000,\nc-seek/00000.m4s\n#EXTINF:2.000000,\nc-seek/00001.m4s\n#EXT-X-ENDLIST\n"
		if err := os.WriteFile(filepath.Join(s.dir, "run-seek.m3u8"), []byte(pl), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		sl := newSlotter(s.base, s.segDur, s.total, 3)
		sl.index = func(at time.Duration) int { return s.plan.IndexAt(at - s.base) }
		doneCh := make(chan struct{})
		close(doneCh)
		r2 := &run{
			seq:      2,
			startSeg: 3,
			maxDone:  -1,
			stderr:   newTailBuffer(40),
			proc:     &fakeProc{done: doneCh, kill: make(chan struct{})},
			spec:     runSpec{Dir: s.dir, Playlist: "run-seek.m3u8", InitName: "init-seek.mp4", ChunkDir: cdir, Start: 6 * time.Second, Decision: d},
			chunks:   &chunkRun{dir: cdir, container: ContainerFMP4, initPath: filepath.Join(s.dir, "init-seek.mp4"), startAt: 6 * time.Second, sl: sl},
		}
		s.cur = r2
		s.mu.Unlock()

		s.collectChunks(r2, true)

		s.mu.Lock()
		failed := r2.chunks.failed
		done3 := s.done[0][3]
		keptInit := s.initFileLocked(0)
		s.mu.Unlock()
		if failed || !done3 {
			t.Fatalf("seek run failed=%v done[3]=%v, want false and true", failed, done3)
		}
		if keptInit != "init-retained.mp4" {
			t.Fatalf("initFile = %q, want retained init-retained.mp4", keptInit)
		}
		seg3 := s.trackSegPath(0, 3)
		// Video tfdt 96000 moved by -(3328 - 1328) = -2000 => 94000.
		if got := readTrackTFDT(t, seg3, 1); got != 94000 {
			t.Fatalf("video tfdt = %d, want 94000", got)
		}
		// Audio tfdt 288000 moved by -(0 - 1024) = +1024 => 289024.
		if got := readTrackTFDT(t, seg3, 2); got != 289024 {
			t.Fatalf("audio tfdt = %d, want 289024", got)
		}
	})

	t.Run("audio-only opening chunk and trailing audio tail", func(t *testing.T) {
		fr := newFakeRunner()
		m := newTestManager(t, fr, config.Transcode{FastStart: true}, withPollInterval(time.Hour))
		info := mustParse(t, "mp4_h264_aac.json")
		info.Duration = 28 * time.Second
		d := core.Decision{Kind: core.DecisionTranscode, Container: ContainerFMP4, VideoCopy: true, AudioCopy: false, AudioCodec: "aac", AudioStream: 1}
		hs, err := m.StartHLS(t.Context(), core.HLSRequest{SessionID: "issue417-audio-tail", InputURL: "http://127.0.0.1:1/src.mkv", Info: info, Decision: d})
		if err != nil {
			t.Fatal(err)
		}
		s := hs.(*Session)
		if s.segDur != 2*time.Second || s.total != 14 {
			t.Fatalf("segDur=%v total=%d, want 2s and 14", s.segDur, s.total)
		}
		stopInitialRun(s)

		s.mu.Lock()
		cdir := filepath.Join(s.dir, "c-tail")
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.dir, "init-tail.mp4"), makeDualInit(0, true, 0), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		// Chunk 0 at 20s (segment 10): audio-only (no video traf), 2s (94 * 1024 / 48000 ~= 2.005s).
		if err := os.WriteFile(filepath.Join(cdir, "00000.m4s"), makeFrag(1, false, 0, 0, 0, true, 20*48000, 94), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		// Chunk 1 at 22s (segment 11): video ends after 0.5s (#EXTINF:0.500000),
		// while audio continues 6s (282 * 1024 / 48000 = 6.016s) to 28s (through segment 13).
		if err := os.WriteFile(filepath.Join(cdir, "00001.m4s"), makeFrag(2, true, 22*16000, 0, 12, true, 22*48000, 282), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		plLive := "#EXTM3U\n#EXTINF:2.000000,\nc-tail/00000.m4s\n"
		if err := os.WriteFile(filepath.Join(s.dir, "run-tail.m3u8"), []byte(plLive), 0o644); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
		sl := newSlotter(s.base, s.segDur, s.total, 10)
		sl.index = func(at time.Duration) int { return s.plan.IndexAt(at - s.base) }
		doneCh := make(chan struct{})
		close(doneCh)
		rt := &run{
			seq:      2,
			startSeg: 10,
			maxDone:  -1,
			stderr:   newTailBuffer(40),
			proc:     &fakeProc{done: doneCh, kill: make(chan struct{})},
			spec:     runSpec{Dir: s.dir, Playlist: "run-tail.m3u8", InitName: "init-tail.mp4", ChunkDir: cdir, Start: 20 * time.Second, Decision: d},
			chunks:   &chunkRun{dir: cdir, container: ContainerFMP4, initPath: filepath.Join(s.dir, "init-tail.mp4"), startAt: 20 * time.Second, sl: sl},
		}
		s.cur = rt
		s.mu.Unlock()

		// While producer is still alive, audio-only chunk 00000.m4s must be consumed immediately, not wedged at cr.next=0.
		s.collectChunks(rt, false)
		if rt.chunks.next != 1 {
			t.Fatalf("audio-only opening chunk wedged collector: cr.next=%d, want 1", rt.chunks.next)
		}

		// Producer exits after writing 00001.m4s with short video #EXTINF and 6s trailing audio.
		plEnd := plLive + "#EXTINF:0.500000,\nc-tail/00001.m4s\n#EXT-X-ENDLIST\n"
		if err := os.WriteFile(filepath.Join(s.dir, "run-tail.m3u8"), []byte(plEnd), 0o644); err != nil {
			t.Fatal(err)
		}
		s.collectChunks(rt, true)

		s.mu.Lock()
		for seg := 10; seg < s.total; seg++ {
			if !s.done[0][seg] {
				t.Errorf("tail segment %d not published", seg)
			}
		}
		s.mu.Unlock()
	})
}
