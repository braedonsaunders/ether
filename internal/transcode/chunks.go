package transcode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Chunked HLS runs.
//
// A session whose plan was built from the source's keyframe index
// (Timeline.Keyframed) cannot let ffmpeg's segmenters choose the cuts:
// they cut at the first keyframe at or after start+k*segment_length, with
// k counting the cuts made, and a restart (seek, failover, rung switch)
// resets k. With 2.5s keyframes and 4s segments that makes 5,5,2.5,...
// segments where the plan lists 5,5,5,...; with 10s keyframes it makes
// fewer segments than the playlist advertises and the tail 404s. A copy
// restart also begins at the keyframe before its seek point.
//
// Such runs instead cut at every keyframe into a chunk directory, and the
// session assigns each chunk to the plan segment its first timestamp falls
// in (the slotter shared with DASH), concatenates the chunks of a segment
// and drops the keyframe interval a remux restart reads before its seek
// point. Encoded runs force their keyframes at the plan's boundaries.
//
// Copies without a keyframe index are chunked the same way onto their
// nominal plan: every segment starts on a keyframe, so it decodes on its
// own after a seek; a segment inside a GOP longer than a segment repeats
// that GOP (an empty Cluster for Matroska).

// chunkCutFor is the chunked muxer segment length for a session segment
// length: a fraction of a segment, at most half a second.
func chunkCutFor(segDur time.Duration) time.Duration {
	c := segDur / 8
	if c > 500*time.Millisecond {
		c = 500 * time.Millisecond
	}
	if c < 40*time.Millisecond {
		c = 40 * time.Millisecond
	}
	return c
}

// maxForcedKeySegments bounds the boundaries one chunked encode forces
// keyframes at (one command line argument); the run chains a new one
// there.
const maxForcedKeySegments = 400

// tsMuxDelay is what ffmpeg's MPEG-TS muxer adds to every timestamp
// (2 x the default 0.7s -muxdelay) unless mpegts_copyts is set.
const tsMuxDelay = 1400 * time.Millisecond

// chunkRun is one chunked run's bookkeeping, owned by its watch goroutine.
type chunkRun struct {
	dir       string // ffmpeg writes chunks here
	container string
	initPath  string        // fMP4: the run's init segment
	startAt   time.Duration // media start of the run's first segment
	next      int           // playlist entries consumed
	sl        *slotter
	vTrack    uint32 // fMP4 video track and timescale, once read
	vScale    uint32
	vShift    int64  // fMP4 video edit list shift (trackInit.EditShift)
	vBad      bool   // fMP4 video edit list unreadable
	mkvVideo  uint64 // Matroska video TrackNumber and NAL length size, once read
	mkvNALLen int
	mkvRead   bool
	// The published Matroska segment a player continuing into the run
	// holds (heldSeg), once read: its last video frame and the stream it
	// came from (mkvSource).
	heldRead, heldOK bool
	heldSeg          int
	heldEnd          time.Duration
	heldSource       string
	// The published segment whose media the run's preroll repeated.
	prerollOK  bool
	prerollSeg int
	failed     bool // output that cannot be published: see failChunkRun
}

// chunkStart is the media time a chunk starts at (its first video
// timestamp), in the run's -ss clock.
func (cr *chunkRun) chunkStart(path string) (time.Duration, error) {
	switch cr.container {
	case ContainerFMP4:
		if cr.vScale == 0 {
			b, err := os.ReadFile(cr.initPath)
			if err != nil {
				return 0, err
			}
			tracks, err := parseInitTracks(b)
			if err != nil {
				return 0, err
			}
			v := tracks[0]
			for _, t := range tracks {
				if t.Handler == "vide" {
					v = t
					break
				}
			}
			cr.vTrack, cr.vScale, cr.vShift, cr.vBad = v.TrackID, v.Timescale, v.EditShift, v.EditBad
		}
		if cr.vBad {
			return 0, errors.New("fmp4: unreadable video edit list")
		}
		// The edit list is the run's own: a restart starting on an
		// open-GOP keyframe has a larger first composition offset.
		ct, err := fmp4ChunkStart(path, cr.vTrack, cr.vScale)
		return ct - ticksToDuration(cr.vShift, cr.vScale), err
	case ContainerMatroska:
		if !cr.mkvRead {
			// Unreadable: Cluster Timestamps, as without a video track.
			cr.mkvVideo, cr.mkvNALLen, _ = mkvVideoTrack(cr.initPath)
			cr.mkvRead = true
		}
		return mkvChunkStart(path, cr.mkvVideo)
	default:
		return tsChunkStart(path)
	}
}

// tsChunkStart reads the first video PES timestamp of an MPEG-TS chunk
// (the first PES of any stream when there is no video PES).
func tsChunkStart(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	const pkt = 188
	buf := make([]byte, 512*pkt)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, err
	}
	buf = buf[:n]
	first := int64(-1)
	for off := 0; off+pkt <= len(buf); {
		p := buf[off : off+pkt]
		if p[0] != 0x47 {
			off++ // resync
			continue
		}
		off += pkt
		if p[1]&0x40 == 0 { // not a payload unit start
			continue
		}
		afc := (p[3] >> 4) & 3
		o := 4
		if afc&2 != 0 {
			o += 1 + int(p[4])
		}
		if afc&1 == 0 || o+14 > pkt {
			continue
		}
		pes := p[o:]
		if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 || pes[7]&0x80 == 0 {
			continue
		}
		t := pes[9:14]
		pts := int64(t[0]>>1&7)<<30 | int64(t[1])<<22 | int64(t[2]>>1)<<15 | int64(t[3])<<7 | int64(t[4]>>1)
		if id := pes[3]; id >= 0xE0 && id <= 0xEF {
			return ptsToMedia(pts), nil
		}
		if first < 0 {
			first = pts
		}
	}
	if first >= 0 {
		return ptsToMedia(first), nil
	}
	return 0, fmt.Errorf("mpegts: no timestamp in %s", filepath.Base(path))
}

func ptsToMedia(pts int64) time.Duration {
	return time.Duration(pts)*time.Second/90000 - tsMuxDelay
}

// fmp4ChunkStart reads the composition time of track's first sample (the
// earliest over all tracks when track is 0) in a fragmented MP4 chunk: its
// decode time plus its composition offset. A chunk starts on a keyframe,
// and an open-GOP keyframe (HEVC CRA) decodes before the leading pictures
// it presents after, so its decode time is early by more than the
// stream's reorder delay.
func fmp4ChunkStart(path string, track, timescale uint32) (time.Duration, error) {
	if timescale == 0 {
		return 0, errors.New("fmp4: no timescale")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		if _, err := f.ReadAt(hdr[:8], off); err != nil {
			return 0, err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:]))
		typ := string(hdr[4:8])
		switch boxSize {
		case 1:
			if _, err := f.ReadAt(hdr[8:16], off+8); err != nil {
				return 0, err
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:]))
		case 0:
			boxSize = size - off
		}
		if boxSize < 8 || boxSize > size-off {
			return 0, fmt.Errorf("fmp4: bad %q box in %s", typ, filepath.Base(path))
		}
		if typ != "moof" {
			off += boxSize
			continue
		}
		if boxSize > maxMoofSize {
			return 0, fmt.Errorf("fmp4: moof of %d bytes", boxSize)
		}
		b := make([]byte, boxSize)
		if _, err := f.ReadAt(b, off); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		var best int64
		found := false
		top, _ := readBoxes(b, 0, len(b))
		if len(top) == 0 {
			return 0, errShortBox
		}
		trafs, _ := readBoxes(b, top[0].start+top[0].hdr, top[0].end)
		for _, traf := range trafs {
			if traf.typ != "traf" {
				continue
			}
			var id uint32
			base := int64(-1)
			var cto int64
			haveRun, badRun := false, false
			kids, _ := readBoxes(b, traf.start+traf.hdr, traf.end)
			for _, k := range kids {
				p := k.payload(b)
				switch {
				case k.typ == "tfhd" && len(p) >= 8:
					id = binary.BigEndian.Uint32(p[4:])
				case k.typ == "tfdt" && len(p) >= 8:
					if p[0] == 1 && len(p) >= 12 {
						base = int64(binary.BigEndian.Uint64(p[4:]))
					} else {
						base = int64(binary.BigEndian.Uint32(p[4:]))
					}
				case k.typ == "trun" && !haveRun:
					var n uint32
					var ok bool
					cto, n, ok = trunFirstOffset(p)
					haveRun, badRun = n > 0, badRun || !ok
				}
			}
			if track != 0 && id != track {
				continue
			}
			if badRun {
				return 0, fmt.Errorf("fmp4: bad trun for track %d in %s", id, filepath.Base(path))
			}
			if base >= 0 && (!found || base+cto < best) {
				best, found = base+cto, true
			}
		}
		if !found {
			return 0, fmt.Errorf("fmp4: no decode time for track %d in %s", track, filepath.Base(path))
		}
		return ticksToDuration(best, timescale), nil
	}
	return 0, fmt.Errorf("fmp4: no fragment in %s", filepath.Base(path))
}

// emptyClusterSize is the size of emptyCluster's output.
const emptyClusterSize = 15

// emptyCluster is a Matroska Cluster holding only its Timestamp (ms).
func emptyCluster(at time.Duration) []byte {
	ms := uint64(max(at, 0) / time.Millisecond)
	b := []byte{0x1F, 0x43, 0xB6, 0x75, 0x8A, 0xE7, 0x88}
	for i := 7; i >= 0; i-- {
		b = append(b, byte(ms>>(8*i)))
	}
	return b
}

// chunkVint reads an EBML variable-length integer: an element ID (marker
// kept) or a size (marker stripped). It returns its length, 0 if invalid.
func chunkVint(b []byte, keepMarker bool) (uint64, int) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0
	}
	l := 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		l++
	}
	if l > 8 || len(b) < l {
		return 0, 0
	}
	v := uint64(b[0])
	if !keepMarker {
		v &= uint64(0xFF >> l)
	}
	for _, c := range b[1:l] {
		v = v<<8 | uint64(c)
	}
	return v, l
}

// assembleChunks makes dst one segment from consecutive chunk files: the
// first chunk renamed, the others appended (fMP4: without their own styp
// and sidx boxes, which describe a single chunk). The chunk files are
// consumed.
func assembleChunks(dst, container string, chunks []fmp4Chunk, remove func(string)) error {
	if len(chunks) == 0 {
		return errors.New("transcode: segment without chunks")
	}
	if len(chunks) == 1 {
		return os.Rename(chunks[0].path, dst)
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	for i, c := range chunks {
		if container == ContainerFMP4 {
			err = copyMP4Boxes(out, c.path, i == 0)
		} else {
			err = appendFile(out, c.path)
		}
		if err != nil {
			return fail(err)
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	for _, c := range chunks {
		remove(c.path)
	}
	return nil
}

func appendFile(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// copyMP4Boxes copies a media chunk's top-level boxes, skipping sidx and
// (unless keepStyp) styp.
func copyMP4Boxes(w io.Writer, path string, keepStyp bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	var hdr [16]byte
	for off := int64(0); off < size; {
		if size-off < 8 {
			return errShortBox
		}
		if _, err := f.ReadAt(hdr[:8], off); err != nil {
			return err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:]))
		typ := string(hdr[4:8])
		switch boxSize {
		case 1:
			if _, err := f.ReadAt(hdr[8:16], off+8); err != nil {
				return err
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:]))
		case 0:
			boxSize = size - off
		}
		if boxSize < 8 || boxSize > size-off {
			return fmt.Errorf("fmp4: bad %q box in %s", typ, filepath.Base(path))
		}
		if typ != "sidx" && (typ != "styp" || keepStyp) {
			if _, err := io.Copy(w, io.NewSectionReader(f, off, boxSize)); err != nil {
				return err
			}
		}
		off += boxSize
	}
	return nil
}

// isPreroll reports whether a run's first chunk c is media an earlier
// segment holds: it ends by the run's first segment, it starts before the
// end of the Matroska GOP a player continuing into the run already holds,
// or its GOP starts in an earlier segment that is already published (a
// restart continuing playback, where repeating the GOP would run the
// stream backwards). Otherwise it is kept, so the run's first segment
// starts on its keyframe. held is the published segment holding a preroll
// chunk's media (-1 when none or unknown). It takes s.mu.
func (s *Session) isPreroll(r *run, cr *chunkRun, c fmp4Chunk) (preroll bool, held int) {
	if !cr.heldRead {
		cr.heldRead = true
		s.heldMKV(r, cr)
	}
	slot := cr.sl.slotOf(c.start)
	s.mu.Lock()
	t := r.track
	if t < 0 || t >= len(s.done) {
		t = 0
	}
	held = -1
	if slot < r.startSeg && s.done[t][slot] {
		held = slot
	}
	s.mu.Unlock()
	switch {
	case c.end <= cr.startAt+cr.sl.tol:
		return true, held
	case cr.heldOK && c.start <= cr.heldEnd:
		if held < 0 {
			held = cr.heldSeg
		}
		return true, held
	case c.start >= cr.startAt-cr.sl.tol:
		return false, -1
	}
	return held >= 0, held
}

// heldMKV is the latest published Matroska segment with media before
// run r's first segment, when a player continuing into the run holds it
// (every segment between is published: empty Clusters inside its GOP),
// and the presentation time of its last video frame. A copy restart's
// keyframes need not match the GOP that segment ends with: an open GOP's
// leading pictures are in the next segment, a release swap brings other
// keyframes, and a GOP longer than a segment runs past r's start. It
// takes s.mu.
func (s *Session) heldMKV(r *run, cr *chunkRun) {
	if cr.container != ContainerMatroska || cr.mkvVideo == 0 {
		return
	}
	s.mu.Lock()
	t := r.track
	if t < 0 || t >= len(s.done) {
		t = 0
	}
	k, path := r.startSeg-1, ""
	for ; k >= 0 && s.done[t][k]; k-- {
		p := s.trackSegPath(t, k)
		st, err := os.Stat(p)
		if err != nil {
			break
		}
		if st.Size() > emptyClusterSize {
			path = p
			break
		}
	}
	source := s.mkvSources[path]
	s.mu.Unlock()
	if path == "" {
		return
	}
	last, ok, err := mkvLastVideo(path, cr.mkvVideo)
	if err != nil || !ok {
		return
	}
	cr.heldOK, cr.heldSeg, cr.heldEnd, cr.heldSource = true, k, last, source
}

// mkvSource identifies the video stream a run writes: a copy of its input,
// or (each encode being its own) the run.
func mkvSource(r *run) string {
	if r.spec.Decision.VideoCopy {
		return "copy " + r.spec.Input
	}
	return fmt.Sprintf("run %d", r.seq)
}

// failChunkRun ends a chunked run whose output cannot be published as it
// is (a keyframe that needed splicing and could not be): nothing more of
// it is published, and it fails with err however ffmpeg exits, through
// the session's usual failure handling (a retry, then the input failure
// handler).
func (s *Session) failChunkRun(r *run, cr *chunkRun, path string, err error) {
	s.log.Warn("HLS run output unusable", "run", r.seq, "err", err)
	s.removals.remove(path)
	cr.failed = true
	r.stderr.add(err.Error())
	s.mu.Lock()
	r.proc = failedProc{Process: r.proc, err: err}
	p := r.proc
	s.mu.Unlock()
	p.Stop()
}

// failedProc is a run's process once its output is unusable.
type failedProc struct {
	Process
	err error
}

func (p failedProc) Err() error {
	<-p.Done()
	return p.err
}

// runVideoCodec is the video codec a run writes.
func runVideoCodec(s runSpec) string {
	if !s.Decision.VideoCopy {
		return s.Decision.VideoCodec
	}
	if v := s.Info.Video(); v != nil {
		return v.Codec
	}
	return ""
}

// chunkSeg is one segment a chunked run finished: assembled into file, or
// (alias >= 0) a copy of an earlier segment whose GOP spans this one too.
type chunkSeg struct {
	n     int
	file  string
	alias int
}

// collectChunks takes the chunks a chunked run listed since the last poll,
// regroups them into the plan's segments and publishes the finished ones.
func (s *Session) collectChunks(r *run, exited bool) {
	s.mu.Lock()
	skip := s.closed || (r.stopped && s.cur != r)
	stopped := r.stopped
	s.mu.Unlock()
	if skip {
		return
	}
	cr := r.chunks
	if cr.failed {
		return
	}
	var outs []slotOut
	p, err := readFFmpegPlaylist(filepath.Join(r.spec.Dir, r.spec.Playlist), exited)
	if err == nil {
		for ; cr.next < len(p.entries); cr.next++ {
			e := p.entries[cr.next]
			path := filepath.Join(cr.dir, e.name)
			if cr.container == ContainerMatroska && !exited {
				// The segment muxer lists a chunk before closing its AVIO
				// buffer. Opening the following numbered file is the fence
				// that proves this chunk is closed. Unlike the HLS muxer,
				// ssegment does not publish chunks with temp_file+rename.
				n, ok := ParseSegmentName(e.name)
				if !ok {
					s.failChunkRun(r, cr, path, fmt.Errorf("transcode: invalid chunk name %q", e.name))
					return
				}
				if _, err := os.Stat(filepath.Join(cr.dir, fmt.Sprintf("%05d.mkv", n+1))); err != nil {
					break
				}
			}
			if s.heldToClock(r) {
				if err := s.moveOntoRetained(r, path); err != nil {
					s.failChunkRun(r, cr, path, fmt.Errorf("transcode: %s on the retained clock: %w", e.name, err))
					return
				}
			}
			start, err := cr.chunkStart(path)
			if err != nil {
				// A transient read must not consume the playlist entry and
				// silently remove a GOP. Retry while the producer is alive;
				// after exit, unusable media takes the normal failover path.
				if !exited {
					break
				}
				s.failChunkRun(r, cr, path, fmt.Errorf("transcode: read chunk %s: %w", e.name, err))
				return
			}
			c := fmp4Chunk{path: path, start: start, end: start + e.dur}
			if !cr.sl.started {
				pre, held := s.isPreroll(r, cr, c)
				if pre {
					// The keyframe interval before the seek point a remux
					// restart reads first: an earlier segment's media.
					s.removals.remove(path)
					if held >= 0 {
						cr.prerollOK, cr.prerollSeg = true, held
					}
					continue
				}
				if cr.heldOK && cr.heldSource != mkvSource(r) && runVideoCodec(r.spec) == "hevc" {
					// A decoder continuing from the held segment goes on
					// into this keyframe from another stream.
					if _, err := mkvSpliceHEVC(path, cr.mkvVideo, cr.mkvNALLen); err != nil {
						s.failChunkRun(r, cr, path, fmt.Errorf("transcode: splice %s: %w", e.name, err))
						return
					}
				}
				if n := cr.sl.slotOf(c.start); n > cr.sl.startSeg && cr.prerollOK {
					// The preroll's GOP spans the run's first segments up
					// to this keyframe: they are inside it, as when the
					// GOP was first published.
					for k := cr.sl.startSeg; k < n; k++ {
						outs = append(outs, slotOut{n: k, alias: cr.prerollSeg})
					}
					cr.sl.startSeg, cr.sl.floor = n, n
				}
			}
			outs = append(outs, cr.sl.add(c)...)
		}
	}
	if exited {
		outs = append(outs, cr.sl.flush(!stopped && r.proc.Err() == nil)...)
	}
	if len(outs) == 0 {
		return
	}
	ext := segmentExt(s.container)
	segs := make([]chunkSeg, 0, len(outs))
	for _, o := range outs {
		if o.alias >= 0 {
			segs = append(segs, chunkSeg{n: o.n, alias: o.alias})
			continue
		}
		file := filepath.Join(cr.dir, fmt.Sprintf("seg-%05d.%s", o.n, ext))
		if err := assembleChunks(file, s.container, o.chunks, s.removals.remove); err != nil {
			s.log.Warn("assemble HLS segment", "run", r.seq, "segment", o.n, "err", err)
			continue
		}
		segs = append(segs, chunkSeg{n: o.n, file: file, alias: -1})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || (r.stopped && s.cur != r) {
		return // the chunk directory goes with the run
	}
	t := r.track
	if t < 0 || t >= len(s.done) {
		t = 0
	}
	if err := s.noteInitLocked(r, t); err != nil {
		for _, sg := range segs {
			if sg.file != "" {
				s.removals.remove(sg.file)
			}
		}
		cr.failed = true
		s.failRunOutputLocked(r, err)
		return
	}
	changed := false
	done := s.done[t]
	for i, sg := range segs {
		n := sg.n
		if n < r.startSeg || (r.planEnd > 0 && n >= r.planEnd) {
			// Past this run's mode boundary: the next run redoes it in its
			// mode.
			if sg.file != "" {
				s.removals.remove(sg.file)
			}
			continue
		}
		dst := s.trackSegPath(t, n)
		switch {
		case done[n]:
			// An earlier run's copy is as good; the run still got here.
			if sg.file != "" {
				s.removals.remove(sg.file)
			}
		case sg.alias >= 0 && s.container == ContainerMatroska:
			// Plex's Matroska player joins fragments into one stream: a
			// repeated GOP would run its clock backwards. An empty Cluster
			// keeps the segment and the stream valid; a request that does
			// not continue the stream (a seek) is served the GOP instead
			// (Session.decodableMKVLocked).
			if os.WriteFile(dst, emptyCluster(s.segStart(n)+matroskaOrigin), 0o644) != nil {
				continue
			}
		case sg.alias >= 0:
			// Players that place media by timestamp get the GOP covering
			// the segment again (as DASH segments do).
			if !done[sg.alias] || linkOrCopy(s.trackSegPath(t, sg.alias), dst) != nil {
				continue
			}
		default:
			if os.Rename(sg.file, dst) != nil {
				s.removals.remove(sg.file)
				continue
			}
			if s.container == ContainerMatroska {
				if s.mkvSources == nil {
					s.mkvSources = map[string]string{}
				}
				s.mkvSources[dst] = mkvSource(r)
			}
		}
		done[n] = true
		r.produced++
		if n > r.maxDone {
			r.maxDone = n
		}
		if t == 0 && sg.alias < 0 {
			// Not an alias: its GOP's size over one segment would look
			// like a bitrate spike to the governor.
			s.noteSegSizeLocked(n, dst)
		}
		changed = true
		if s.leadOverLocked(r) {
			// One poll can list hundreds of segments once VideoToolbox is
			// ahead. Stop publishing at the throttle window so those files
			// never become the only ones left on disk.
			s.throttleLocked()
			for _, rest := range segs[i+1:] {
				if rest.file != "" {
					s.removals.remove(rest.file)
				}
			}
			break
		}
	}
	if changed {
		s.progressedLocked(r)
	}
}
