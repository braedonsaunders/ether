package transcode

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ServeSegment implements core.HLSSession. n == InitSegment serves the fMP4
// init segment. For DASH sessions it serves the first representation.
func (s *Session) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	if n == InitSegment {
		s.serveInit(w, r, 0)
		return
	}
	s.serveSegment(w, r, 0, n)
}

// serveSegment writes segment n of track t, waiting for ffmpeg and
// restarting it when n is behind or far ahead of the encoder.
func (s *Session) serveSegment(w http.ResponseWriter, r *http.Request, t, n int) {
	s.Touch()
	if t < 0 || t >= len(s.done) || n < 0 || (s.total > 0 && n >= s.total) {
		http.NotFound(w, r)
		return
	}
	// A long wait keeps bytes flowing to the player (segment_padding.go).
	var pw *paddedSegmentWriter
	var padC <-chan time.Time
	if unit := segmentPadding(s.container); unit != nil && !s.isDASH() && r.Method == http.MethodGet && r.Header.Get("Range") == "" {
		pw = &paddedSegmentWriter{ResponseWriter: w, unit: unit}
		w = pw
		defer pw.finish()
		tick := time.NewTicker(segmentPadEvery)
		defer tick.Stop()
		padC = tick.C
	}
	waitStart := time.Now()
	timeout := time.NewTimer(s.m.opts.segmentTimeout)
	defer timeout.Stop()
	restarts := 0
	s.mu.Lock()
	s.reqSeq++
	seq := s.reqSeq
	s.mu.Unlock()
	tracked := false
	defer func() {
		if !tracked {
			return
		}
		s.mu.Lock()
		s.endFetchLocked(n)
		s.mu.Unlock()
	}()
	for {
		if r.Context().Err() != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			http.Error(w, "session closed", http.StatusGone)
			return
		}
		stale := seq < s.movedBy
		if stale && tracked {
			// A newer seek owns the encoder. Drop this fetch so it cannot
			// keep the playhead on the position the player left.
			s.endFetchLocked(n)
			tracked = false
		}
		if !stale && r.Method == http.MethodGet && s.tracksPlayheadLocked(t) {
			if !tracked {
				s.beginFetchLocked(t, n)
				tracked = true
			} else {
				s.notePlayheadLocked(t, n)
			}
		}
		paths, availability := s.segmentFilesLocked(t, n, r.Method)
		if availability == segmentRetry {
			s.mu.Unlock()
			continue
		}
		if availability == segmentReady {
			// The segment is already on disk. A throttle stop still resumes
			// here so the encoder continues ahead of this playhead.
			s.throttleLocked()
			s.pruneLocked()
			s.mu.Unlock()
			if s.serveFiles(w, r, paths, t, n) {
				pw.delivered(paths)
				return
			}
			s.mu.Lock()
			delete(s.done[t], n) // pruned during delivery: produce again
			s.mu.Unlock()
			continue
		}
		waitTail := availability == segmentFinalizing

		if s.inputFailed {
			s.mu.Unlock()
			writeStartError(w, errInputExhausted)
			return
		}
		if s.endSeg >= 0 && n > s.endSeg && (s.total == 0 || n >= s.total) {
			// A known VOD playlist still listing n is not past the
			// title. A previous run's window EOF must restart, not 404.
			s.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		// Cached media requires no encoder work. A delayed request for an
		// old representation must not switch the current video encoder.
		// Audio shares the active video run and never chooses its rung.
		previousTrack := s.wantTrack
		if !waitTail && !stale && r.Context().Err() == nil && r.Method == http.MethodGet {
			if !s.isDASH() || t != s.audioTrackLocked() {
				s.wantTrack = t
				if s.isDASH() {
					s.videoRequested = true
				}
			}
		}
		if r.Method != http.MethodGet &&
			(s.cur == nil || s.cur.track != t) {
			s.mu.Unlock()
			http.Error(w, "rendition not active", http.StatusServiceUnavailable)
			return
		}
		var dwell *time.Timer
		// Only the oldest outstanding fetch may move the encoder. A request
		// for a segment the encoder has already raced to is a buffer fill,
		// not a seek, and must not abandon the segment the client is on.
		if !waitTail && !s.mayMoveEncoderLocked(t, n, tracked) && s.needsRestartLocked(n) {
			s.throttleLocked()
			s.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "segment is ahead of the playhead", http.StatusServiceUnavailable)
			return
		}
		if !waitTail && s.mayMoveEncoderLocked(t, n, tracked) && s.needsRestartLocked(n) {
			// The cap covers the empty position and what follows it, never
			// a segment before it: a seek back there still has media.
			if s.emptyRuns >= emptyRestartCap && n >= s.emptyAt && (s.lastReal < 0 || n > s.lastReal) && !s.segmentPublishedLocked(t, n) {
				s.mu.Unlock()
				s.log.Warn("segment unavailable after empty restarts", "segment", n, "lastReal", s.lastReal, "emptyAt", s.emptyAt)
				http.Error(w, "segment unavailable", http.StatusBadGateway)
				return
			}
			if stale {
				// Superseded by a seek: answer now instead of moving the
				// encoder away from where the player went. A player that
				// still wants this segment retries as a new request.
				s.mu.Unlock()
				w.Header().Set("Retry-After", "1")
				http.Error(w, "superseded by a newer request", http.StatusServiceUnavailable)
				return
			}
			if left := s.rungDwellLocked(r.Method == http.MethodGet && r.Header.Get("Range") == "", n); left > 0 {
				// Another rung's run is inside its dwell: wait it out
				// instead of restarting the encoder now.
				s.wantTrack = previousTrack // pending/canceled probes must not steer chaining
				s.throttleLocked()
				dwell = time.NewTimer(left)
			} else {
				// Starting at n. A throttle resume would first start at the
				// old lead (the copy ran ahead and stopped), and this request
				// would start again. One rung switch counted as two runs.
				if restarts >= 2 {
					err := s.failErr
					s.mu.Unlock()
					s.log.Warn("segment unavailable after restarts", "segment", n, "err", err)
					http.Error(w, "segment unavailable", http.StatusBadGateway)
					return
				}
				restarts++
				s.seekRampSeg = n
				s.movedBy = seq
				why := "fetch ahead"
				if s.cur != nil && (n < s.cur.startSeg || n < s.cur.next()) {
					why = "fetch behind"
				}
				if err := s.startRunForLocked(n, s.failures > 0, why); err != nil {
					s.mu.Unlock()
					s.log.Error("start ffmpeg", "segment", n, "err", err)
					writeStartError(w, err)
					return
				}
			}
		} else {
			s.throttleLocked()
		}
		ch := s.changed
		s.mu.Unlock()
		var dwellC <-chan time.Time
		if dwell != nil {
			dwellC = dwell.C
		}
		for padded := true; padded; {
			padded = false
			select {
			case <-ch:
			case <-dwellC:
			case <-padC:
				// Padding is not progress: keep waiting for the same change.
				waited := time.Since(waitStart)
				if pw.committed && waited >= s.m.opts.segmentPadLimit {
					stopTimer(dwell)
					s.log.Warn("segment still not ready after padding; aborting the request", "segment", n, "waited", waited)
					return // finish aborts the committed response
				}
				if pw.committed || waited >= segmentPadAfter {
					pw.pad(s)
				}
				padded = true
			case <-r.Context().Done():
				stopTimer(dwell)
				return
			case <-timeout.C:
				stopTimer(dwell)
				s.log.Warn("timed out waiting for segment", "segment", n)
				http.Error(w, "timed out waiting for segment", http.StatusGatewayTimeout)
				return
			}
		}
		stopTimer(dwell)
	}
}

// emptyAliasSize reports whether an in-GOP placeholder segment is written
// for HLS containers whose players join fragments into a single stream: a
// positive size when placeholders exist, 0 when aliases duplicate the
// covering segment directly (MPEG-TS, DASH). The fMP4 placeholder's exact
// size varies with the track, so it is matched structurally (isEmptyFMP4),
// not by size.
func (s *Session) emptyAliasSize() int64 {
	if s.isDASH() {
		return 0
	}
	switch s.container {
	case ContainerMatroska:
		return emptyClusterSize
	case ContainerFMP4:
		return 1
	default:
		return 0
	}
}

// emptyFragmentLocked is the placeholder bytes for in-GOP segment n of the
// primary track: an empty fMP4 fragment (with a valid traf/tfdt at n's start)
// or an empty Matroska Cluster. Callers write it in place of repeating the
// covering GOP.
func (s *Session) emptyFragmentLocked(n int) []byte {
	if s.container == ContainerMatroska {
		return emptyCluster(s.segStart(n) + matroskaOrigin)
	}
	track := s.fmp4VideoTrack
	base := emptyFMP4DecodeTime(s.segStart(n), track.Timescale)
	return emptyFMP4(uint32(n+1), track, base)
}

func isEmptyFMP4(b []byte) bool {
	top, err := readBoxes(b, 0, len(b))
	if err != nil || len(top) != 3 || top[0].typ != "styp" || top[1].typ != "moof" || top[2].typ != "mdat" {
		return false
	}
	if top[2].end-top[2].start > 8 {
		return false // mdat with samples is not an empty placeholder
	}
	kids, err := readBoxes(b, top[1].start+top[1].hdr, top[1].end)
	if err != nil {
		return false
	}
	sawTfdt := false
	for _, k := range kids {
		if k.typ != "traf" {
			continue
		}
		grandkids, err := readBoxes(b, k.start+k.hdr, k.end)
		if err != nil {
			return false
		}
		samples := 0
		for _, g := range grandkids {
			switch g.typ {
			case "tfdt":
				sawTfdt = true
			case "trun":
				p := g.payload(b)
				if len(p) >= 8 {
					samples += int(binary.BigEndian.Uint32(p[4:]))
				}
			}
		}
		if samples != 0 {
			return false
		}
	}
	return sawTfdt
}

// coveringSegLocked is the latest segment at or before n that holds a
// real GOP (not an empty alias placeholder), or -1 if none is still on disk.
func (s *Session) coveringSegLocked(t, n int) int {
	if t < 0 || t >= len(s.done) {
		return -1
	}
	for k := n; k >= 0 && s.done[t][k]; k-- {
		path := s.trackSegPath(t, k)
		st, err := os.Stat(path)
		if err != nil {
			return -1
		}
		switch s.container {
		case ContainerMatroska:
			if st.Size() > emptyClusterSize {
				return k
			}
		case ContainerFMP4:
			b, err := os.ReadFile(path)
			if err != nil {
				return -1
			}
			if !isEmptyFMP4(b) {
				return k
			}
		default:
			if st.Size() > 0 {
				return k
			}
		}
	}
	return -1
}

// decodableMKVLocked is the file to serve for segment n of track t to a
// fresh decoder: n itself, or for an empty alias placeholder (a segment
// inside a GOP longer than a segment, chunks.go) the earlier segment
// holding that GOP; false when it is no longer on disk.
func (s *Session) decodableMKVLocked(t, n int) (string, bool) {
	if k := s.coveringSegLocked(t, n); k >= 0 {
		return s.trackSegPath(t, k), true
	}
	return "", false
}

func stopTimer(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}

// tracksPlayheadLocked reports whether a fetch of track t shares the
// timeline index. DASH audio uses the same segment numbers as video.
func (s *Session) tracksPlayheadLocked(int) bool { return true }

// beginFetchLocked records a GET and places the playhead.
func (s *Session) beginFetchLocked(t, n int) {
	if s.inflight == nil {
		s.inflight = map[int]int{}
	}
	s.inflight[n]++
	s.notePlayheadLocked(t, n)
	if s.log != nil {
		s.log.Info("segment fetch", "segment", n, "playhead", s.lastReq, "oldest", s.oldestFetchLocked())
	}
}

// endFetchLocked drops a finished or abandoned GET and lets a waiting seek
// take the playhead.
func (s *Session) endFetchLocked(n int) {
	if s.inflight[n] > 1 {
		s.inflight[n]--
	} else {
		delete(s.inflight, n)
	}
	prev := s.lastReq
	if oldest := s.oldestFetchLocked(); oldest >= 0 && !s.encoderHeadLocked(oldest) {
		s.lastReq = oldest
	}
	if s.lastReq != prev {
		s.broadcastLocked()
	}
}

// notePlayheadLocked follows the oldest fetch still open. A segment the
// encoder has not produced and will not reach is a seek: that request
// becomes the playhead even if an older one is still open, and the older
// one is then superseded. A fetch of a segment the encoder has already
// reached is the encoder head, not a seek, so it cannot pull eviction
// up to that head.
func (s *Session) notePlayheadLocked(t, n int) {
	prev := s.lastReq
	published := s.segmentPublishedLocked(t, n)
	restart := s.needsRestartLocked(n)
	head := s.encoderHeadLocked(n)
	decision := "hold"
	if !published && restart && !head {
		s.lastReq = n
		decision = "seek"
	} else if oldest := s.oldestFetchLocked(); oldest >= 0 && !s.encoderHeadLocked(oldest) {
		s.lastReq = oldest
		decision = "oldest"
	}
	if s.traceOn() {
		s.log.Info("playhead", "segment", n, "decision", decision, "prev", prev, "playhead", s.lastReq, "oldest", s.oldestFetchLocked(), "published", published, "needsRestart", restart, "encoderHead", head)
	}
	if s.lastReq > prev+3 || s.lastReq < prev-3 {
		s.recoverySeekAt = time.Now()
		s.completedDeliveries = nil
		if s.changed != nil {
			s.broadcastLocked()
		}
	}
}

// encoderHeadLocked reports that n is at or behind the encoder and far
// enough ahead of the playhead that adopting it would evict the segment
// the client is still on. A real seek lands beyond the encoder.
func (s *Session) encoderHeadLocked(n int) bool {
	// With throttling off, the segment a client fetches is the playhead
	// even when the encoder has already passed it.
	if s.m == nil || s.m.opts.maxAhead <= 0 || s.segDur <= 0 {
		return false
	}
	if n <= s.lastReq+s.restartLimit() {
		return false
	}
	for t := range s.done {
		if s.segmentPublishedLocked(t, n) {
			return true
		}
	}
	r := s.cur
	if r == nil || r.finished || r.stopped || r.maxDone < 0 {
		return false
	}
	tip := r.maxDone
	if next := r.next(); next-1 > tip {
		tip = next - 1
	}
	return n <= tip+s.restartLimit()
}

func (s *Session) segmentPublishedLocked(t, n int) bool {
	if t < 0 || t >= len(s.done) {
		return false
	}
	return s.done[t][n]
}

// advancePlayheadLocked follows contiguous delivery. A segment fetched far
// ahead of the playhead, or while an earlier fetch is still open, does not
// move eviction or the encoder throttle.
func (s *Session) advancePlayheadLocked(n int) {
	if n < s.lastReq || n > s.lastReq+s.restartLimit() {
		return
	}
	if oldest := s.oldestFetchLocked(); oldest >= 0 && n > oldest {
		return
	}
	s.lastReq = n
}

func (s *Session) oldestFetchLocked() int {
	oldest := -1
	for n, c := range s.inflight {
		if c <= 0 {
			continue
		}
		if oldest < 0 || n < oldest {
			oldest = n
		}
	}
	return oldest
}

// mayMoveEncoderLocked reports whether this request is the playhead fetch.
// A newer segment request waits; it does not restart ffmpeg at the encoder head.
func (s *Session) mayMoveEncoderLocked(t, n int, tracked bool) bool {
	if !tracked {
		return true
	}
	if !s.segmentPublishedLocked(t, n) && s.needsRestartLocked(n) && !s.encoderHeadLocked(n) {
		return true
	}
	return n == s.oldestFetchLocked()
}

// restartLimit is how many segments ahead of the encoder a request may be
// before it is a seek.
func (s *Session) restartLimit() int {
	ahead := 2
	if s.m != nil && s.segDur > 0 {
		if n := int(s.m.opts.restartAhead / s.segDur); n > ahead {
			ahead = n
		}
	}
	return ahead
}

// needsRestartLocked decides between waiting for the running ffmpeg and
// restarting it at n: wait when n is at or a little ahead of the encoder,
// restart when n is behind the run's start, far ahead, or nothing runs.
func (s *Session) needsRestartLocked(n int) bool {
	if s.liveRecovering {
		return false
	}
	if s.pendingStart && s.pendingSegment == n && s.pendingTrack == s.wantTrack {
		return false
	}
	r := s.cur
	if r == nil || r.finished || r.stopped {
		return true
	}
	if r.track != s.wantTrack {
		return true // the client switched rungs: re-encode from n there
	}
	if n < r.startSeg || n < r.next() {
		// Behind the run's start, or already passed by it (pruned).
		return true
	}
	return n > r.next()+s.restartLimit()
}

// rungDwellLocked protects the sole encoder from rendition probes. Completed
// delivery followed by the next media GET is playback progress, in either
// direction. Holding an upshift for eight seconds after delivering two seconds
// of video starves the player just as surely as holding a downshift.
func (s *Session) rungDwellLocked(media bool, n int) time.Duration {
	r := s.cur
	if r == nil || r.finished || r.stopped || r.err != nil || r.track == s.wantTrack || s.m.opts.rungDwell <= 0 {
		return 0
	}
	// The eager base encoder is not yet a player choice. Honor the first
	// requested rendition immediately, including init-first HLS startup.
	// A restart increments runSeq, so this cannot excuse repeated probes.
	if s.runSeq == 1 && s.deliveredCount == 0 {
		return 0
	}
	if s.deliveredTrack == r.track && s.deliveredCount > 0 {
		if media && n == s.deliveredSeg+1 {
			return 0
		}
		// fMP4/DASH players load initialization before the next media GET.
		// Permit one such switch after delivery on the active track. Once
		// switched, that evidence belongs to the old track, so further init
		// probes still wait until the new track actually delivers media.
		if !media {
			return 0
		}
	}
	if left := s.m.opts.rungDwell - s.goodput.now().Sub(r.started); left > 0 {
		return left
	}
	return 0
}

// serveInit writes the init segment of track t, waiting for ffmpeg to
// write it.
func (s *Session) serveInit(w http.ResponseWriter, r *http.Request, t int) {
	s.Touch()
	if !hasInitSegment(s.container) || t < 0 || t >= len(s.done) {
		http.NotFound(w, r)
		return
	}
	timeout := time.NewTimer(s.m.opts.segmentTimeout)
	defer timeout.Stop()
	restarted := false
	for {
		if r.Context().Err() != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			http.Error(w, "session closed", http.StatusGone)
			return
		}
		if name := s.initFileLocked(t); name != "" {
			// Open under the lock: a newer run's init may replace (and
			// delete) this one as soon as the lock is released, and the
			// open handle keeps serving the bytes the lookup chose.
			f, err := os.Open(filepath.Join(s.dir, name))
			if err == nil && s.emptyAliasSize() > 0 && r.Method != http.MethodHead {
				// An init GET starts a decoder. Its first media GET needs
				// a GOP even when it follows the previous decoder's last
				// segment; metadata-only HEAD probes do not reset it.
				delete(s.mkvDelivery, t)
			}
			s.mu.Unlock()
			if err == nil {
				defer f.Close()
				if s.serveOpened(w, r, f, t, InitSegment) {
					return
				}
			}
			http.Error(w, "init segment missing", http.StatusInternalServerError)
			return
		}
		if s.inputFailed {
			s.mu.Unlock()
			writeStartError(w, errInputExhausted)
			return
		}
		// A DASH player requests initialization before media. Selecting a
		// lazy video representation must start it here; waiting for its
		// first media request otherwise deadlocks startup. Audio shares the
		// current video run and must never select another video rung.
		// Native FFmpeg-based players open init resources with Range: bytes=0-.
		// These requests must wait for the running encoder just like a normal
		// GET; rejecting them while the header is being written breaks cold
		// playback. They may observe the active run, but never select or start
		// another rendition merely to answer a probe.
		//
		// Exception: for HLS, a ranged GET is how native players always open
		// the EXT-X-MAP init, so it follows the lazy-selection path below like
		// an unranged GET (wantTrack, dwell, startRunLocked). Treating it as
		// a probe deadlocked every lazy rung: the player asks for no media
		// segment until the init succeeds. Only DASH keeps the ranged-probe
		// guard; non-GET requests stay probes on both protocols.
		probe := r.Method != http.MethodGet || (s.isDASH() && r.Header.Get("Range") != "")
		selectTrack := !s.isDASH() || t != s.audioTrackLocked()
		if probe && (s.cur == nil || s.cur.finished || s.cur.stopped || (selectTrack && s.cur.track != t)) {
			s.mu.Unlock()
			http.Error(w, "initialization not ready", http.StatusServiceUnavailable)
			return
		}
		previousTrack := s.wantTrack
		if selectTrack && !restarted && !probe {
			s.wantTrack = t
		}
		var dwell *time.Timer
		if (s.cur == nil || s.cur.finished || s.cur.stopped || (selectTrack && s.cur.track != t)) && !restarted && !probe {
			left := s.rungDwellLocked(false, s.lastReq)
			if s.isDASH() && selectTrack && !s.videoRequested && s.runSeq <= 1 {
				// No video was requested yet: the initial quality choice
				// has no active rendition to protect with a dwell.
				left = 0
			}
			if left > 0 {
				// A rendition probe for another rung's init segment waits
				// out the running rung's dwell (see rungDwellLocked).
				s.wantTrack = previousTrack // pending/canceled probes must not steer chaining
				dwell = time.NewTimer(left)
			} else {
				restarted = true
				start := s.lastReq
				// The rounded final DASH slot may contain only trailing
				// audio. An init-only rendition switch still needs video
				// headers, so start one slot earlier and let the slotter
				// publish the tail alias from that run.
				if s.isDASH() && s.total > 1 && start >= s.total-1 {
					start = s.total - 2
				}
				if start < 0 {
					start = 0
				}
				if err := s.startRunForLocked(start, s.failures > 0, "rendition init"); err != nil {
					s.mu.Unlock()
					writeStartError(w, err)
					return
				}
			}
		}
		ch := s.changed
		s.mu.Unlock()
		var dwellC <-chan time.Time
		if dwell != nil {
			dwellC = dwell.C
		}
		select {
		case <-ch:
		case <-dwellC:
		case <-r.Context().Done():
			stopTimer(dwell)
			return
		case <-timeout.C:
			stopTimer(dwell)
			http.Error(w, "timed out waiting for init segment", http.StatusGatewayTimeout)
			return
		}
		stopTimer(dwell)
	}
}

// tailSettledLocked reports whether nothing more will follow segment n of
// the current input: ffmpeg hit EOF, moved past n, or is not running.
func (s *Session) tailSettledLocked(n int) bool {
	if s.endSeg >= 0 {
		return true
	}
	r := s.cur
	return r == nil || r.finished || r.maxDone > n
}

// serveFiles writes one segment made of one or more files.
func (s *Session) serveFiles(w http.ResponseWriter, r *http.Request, paths []string, t, n int) bool {
	if len(paths) == 1 {
		return s.serveFile(w, r, paths[0], t, n)
	}
	var buf bytes.Buffer
	var mod time.Time
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		buf.Write(b)
		if st, err := os.Stat(p); err == nil && st.ModTime().After(mod) {
			mod = st.ModTime()
		}
	}
	s.setSegmentHeaders(w)
	armSegmentDeadline(w)
	s.serveCounted(w, r, filepath.Base(paths[0]), mod, bytes.NewReader(buf.Bytes()), t, n)
	return true
}

// serveCounted is http.ServeContent with delivery goodput measured over
// the write phase only (the wait for ffmpeg already happened).
func (s *Session) serveCounted(w http.ResponseWriter, r *http.Request, name string, mod time.Time, content io.ReadSeeker, t, n int) {
	if r.Method != http.MethodGet || n == InitSegment {
		http.ServeContent(w, r, name, mod, content)
		return
	}
	// Ranged media bytes are also delivery; HEAD and init metadata are not.
	if r.Header.Get("Range") != "" {
		cw := &countingWriter{ResponseWriter: w}
		http.ServeContent(cw, r, name, mod, content)
		if cw.status == http.StatusPartialContent && cw.err == nil && cw.bytes > 0 && r.Context().Err() == nil {
			s.mu.Lock()
			s.playbackDelivered = true
			s.mu.Unlock()
		}
		return
	}
	size, err := content.Seek(0, io.SeekEnd)
	if err != nil {
		http.ServeContent(w, r, name, mod, content)
		return
	}
	if _, err = content.Seek(0, io.SeekStart); err != nil {
		return
	}
	cw := &countingWriter{ResponseWriter: w}
	start := s.goodput.now()
	http.ServeContent(cw, r, name, mod, content)
	dur := s.goodput.now().Sub(start)
	if cw.status != http.StatusOK || cw.err != nil || cw.bytes != size || size == 0 || r.Context().Err() != nil {
		return
	}
	if remoteDelivery(r) {
		s.goodput.add(cw.bytes, dur)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.playbackDelivered = true
	// Audio, retries, stale representations and overlapping requests cannot
	// manufacture sequential video progress or drain another rung's buffer.
	if n >= s.lastReq-3 && n <= s.lastReq+512 {
		if s.completedDeliveries == nil {
			s.completedDeliveries = map[int]map[int]bool{}
		}
		if s.completedDeliveries[t] == nil {
			s.completedDeliveries[t] = map[int]bool{}
		}
		s.completedDeliveries[t][n] = true
		for k := range s.completedDeliveries[t] {
			if k < s.lastReq-3 || k > s.lastReq+512 {
				delete(s.completedDeliveries[t], k)
			}
		}
	}
	if s.tracksPlayheadLocked(t) {
		s.advancePlayheadLocked(n)
	}
	if s.cur != nil && s.cur.track == t && (!s.isDASH() || t != s.audioTrackLocked()) {
		if s.deliveredCount == 0 || s.deliveredTrack != t || n != s.deliveredSeg+1 {
			if s.deliveredCount > 0 && s.deliveredTrack == t && n <= s.deliveredSeg {
				return
			}
			s.deliveredCount = 0
		}
		s.deliveredTrack, s.deliveredSeg = t, n
		s.deliveredCount++
	}
	// React to delivery evidence now, even when encoding is paused ahead.
	s.maybeGovernLocked()
}

// initFileLocked is the current init segment file of track t, relative
// to the session dir ("" none).
func (s *Session) initFileLocked(t int) string {
	if s.isDASH() {
		return s.dash.inits[t]
	}
	if t >= 0 && t < len(s.initFiles) && s.initFiles[t] != "" {
		return s.initFiles[t]
	}
	if t == 0 {
		return s.initFile
	}
	return ""
}

func (s *Session) setSegmentHeaders(w http.ResponseWriter) {
	ct := "video/mp2t"
	switch {
	case s.isDASH():
		ct = "application/octet-stream" // as PMS serves DASH segments
	case s.container == ContainerFMP4:
		ct = "video/mp4"
	case s.container == ContainerMatroska:
		ct = "video/x-matroska"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
}

// serveFile writes a finished segment; false when the file is gone.
func (s *Session) serveFile(w http.ResponseWriter, r *http.Request, path string, t, n int) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return s.serveOpened(w, r, f, t, n)
}

// serveOpened writes segment n (or the init segment) from an open file.
func (s *Session) serveOpened(w http.ResponseWriter, r *http.Request, f *os.File, t, n int) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	s.setSegmentHeaders(w)
	name := filepath.Base(f.Name())
	if n == InitSegment {
		name = initSegmentName(s.container)
	}
	armSegmentDeadline(w)
	s.serveCounted(w, r, name, st.ModTime(), f, t, n)
	return true
}

// segmentWriteTimeout bounds delivering one finished segment. Servers have
// no WriteTimeout (direct play lasts hours), so without it a client that
// stops reading would hold the request goroutine and the open segment file
// (which Windows then cannot delete on session cleanup) forever. A segment
// that takes longer than this is far below playable bitrate anyway.
const segmentWriteTimeout = 5 * time.Minute

// armSegmentDeadline sets the connection write deadline just before a
// segment is written (never before waiting for ffmpeg).
func armSegmentDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(segmentWriteTimeout))
}
