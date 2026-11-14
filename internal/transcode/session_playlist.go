package transcode

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ServePlaylist implements core.HLSSession: the full-length VOD playlist
// (the MPD for DASH sessions).
func (s *Session) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	if s.isDASH() {
		s.ServeManifest(w, r)
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	body := s.renderPlaylistLocked(0)
	s.notePlaylistLocked(0, body)
	if s.androidMatroskaHLS && s.container == ContainerMatroska {
		// Match PMS 1.42.2's Android segmented-Matroska playlist contract
		// (rex subtitles_s1422.json). In particular, do not claim the
		// unindexed copy plan's empty/GOP aliases are independent segments.
		body = bytes.ReplaceAll(body, []byte("#EXT-X-PLAYLIST-TYPE:VOD\n"), nil)
		body = bytes.ReplaceAll(body, []byte("#EXT-X-INDEPENDENT-SEGMENTS\n"), nil)
		body = bytes.Replace(body, []byte("#EXT-X-MEDIA-SEQUENCE:"), []byte("#EXT-X-ALLOW-CACHE:NO\n#EXT-X-MEDIA-SEQUENCE:"), 1)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func (s *Session) span() time.Duration {
	d := s.info.Duration - s.base
	if d < 0 {
		return 0
	}
	return d
}

// renderPlaylistLocked renders track t's media playlist from the plan.
// The primary track's VOD playlist is cached until the plan changes ahead
// of the player (spike governor) or SwapInput; governed sessions render
// an EVENT playlist fresh each time so reloads see re-planned segments.
func (s *Session) renderPlaylistLocked(t int) []byte {
	independent := !s.requireIndependentProof || s.segmentsIndependent
	if s.liveInput && s.total == 0 && !s.isDASH() {
		return s.livePlaylistLocked(t, func(n int) bool { return s.discontBeforeLocked(t, n) })
	}
	if s.total == 0 {
		s.log.Warn("using growing HLS playlist", "reason", "source duration unavailable")
	}
	if s.plan == nil {
		if s.total > 0 {
			return mediaPlaylistWithIndependence(s.span(), s.segDur, s.container, s.segName, independent)
		}
		done := make([]int, 0, len(s.done[0]))
		for n := range s.done[0] {
			done = append(done, n)
		}
		sort.Ints(done)
		return eventPlaylist(done, s.segDur, s.container, s.endSeg >= 0, s.segName)
	}
	discont := func(n int) bool { return s.discontBeforeLocked(t, n) }
	if s.total > 0 {
		if s.governorOn && t == 0 {
			return planPlaylist(s.plan, s.container, s.segName, discont, true, s.endSeg >= 0, 0, independent)
		}
		if t == 0 {
			if s.playlist == nil {
				s.playlist = planPlaylist(s.plan, s.container, s.segName, discont, false, true, 0, independent)
			}
			return s.playlist
		}
		return planPlaylist(s.plan, s.container, s.segName, discont, false, true, 0, independent)
	}
	// Unknown duration: list the contiguous completed prefix.
	return planPlaylist(s.plan, s.container, s.segName, discont, true, s.endSeg >= 0, s.listedLocked(t), independent)
}

// liveWindow is how many segments a live playlist lists. It slides: a live
// session runs for hours, and its consumer (Live TV's owned timeline) reads
// only the newest segments.
const liveWindow = 10

// livePlaylistLocked renders a live session's sliding playlist: the newest
// liveWindow segments of the contiguous completed prefix, each with the
// length ffmpeg actually cut (copy cuts at source keyframes, so lengths
// vary), and EXT-X-DISCONTINUITY-SEQUENCE counting the discontinuities that
// slid out (RFC 8216 section 4.3.3.3; Apple authoring rule 8.17).
func (s *Session) livePlaylistLocked(t int, discont func(int) bool) []byte {
	listed := s.listedLocked(t)
	first := max(0, listed-liveWindow)
	discSeq := 0
	for n := 1; n < first; n++ {
		if discont(n) {
			discSeq++
		}
	}
	target := s.segDur
	for n := first; n < listed; n++ {
		target = max(target, s.liveDurationLocked(n))
	}
	var b bytes.Buffer
	version := 3
	if hasInitSegment(s.container) {
		version = 7
	}
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", version)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(target.Seconds())))
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", first)
	fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", discSeq)
	if hasInitSegment(s.container) {
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", initSegmentName(s.container))
	}
	for n := first; n < listed; n++ {
		if n > 0 && discont(n) {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
			if hasInitSegment(s.container) && n > first {
				fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", initSegmentName(s.container))
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s\n", s.liveDurationLocked(n).Seconds(), s.segName(n))
	}
	if s.endSeg >= 0 {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.Bytes()
}

func (s *Session) liveDurationLocked(n int) time.Duration {
	if d := s.liveDurations[n]; d > 0 {
		return d
	}
	if s.plan != nil {
		return s.plan.Dur(n)
	}
	return s.segDur
}

// noteLiveDurationLocked records segment n's real length, keeping only what
// the sliding window can still list.
func (s *Session) noteLiveDurationLocked(n int, d time.Duration) {
	if d <= 0 {
		return
	}
	if s.liveDurations == nil {
		s.liveDurations = map[int]time.Duration{}
	}
	s.liveDurations[n] = d
	if len(s.liveDurations) > 4*liveWindow {
		for k := range s.liveDurations {
			if k < n-2*liveWindow {
				delete(s.liveDurations, k)
			}
		}
	}
}

// listedLocked advances and returns how many segments track t's
// unknown-duration EVENT playlist lists: the contiguous completed prefix.
// It never shrinks, so pruning segments far behind the player (pruneLocked)
// cannot remove them from the playlist, which EVENT playlists forbid.
func (s *Session) listedLocked(t int) int {
	if t < 0 || t >= len(s.done) {
		t = 0
	}
	for len(s.listed) <= t {
		s.listed = append(s.listed, 0)
	}
	done := s.done[t]
	for done[s.listed[t]] {
		s.listed[t]++
	}
	return s.listed[t]
}

// rampTranscodesLocked reports whether the ramp replaces the first
// segments of a copy stream with a quick transcode (only then does the
// switch back to copy need a discontinuity). fMP4 copy sessions instead
// keep copying (with shorter segments): their init segment would change
// across the switch.
func (s *Session) rampTranscodesLocked() bool {
	return s.decision.VideoCopy && s.container == ContainerMPEGTS && s.info.Video() != nil
}

// discontBeforeLocked: a discontinuity is needed exactly where the encode
// parameters the decoder sees change — ramp encode -> copy, and in/out of
// governor-clamped segments. Same-codec/same-resolution preset changes
// (transcode ramp) need none, matching ordinary seek restarts. Rung
// tracks (t > 0) are steady transcodes of one shape, so only the primary
// track can have discontinuities.
func (s *Session) discontBeforeLocked(t, n int) bool {
	if s.rtxDiscontinuities[n] || s.liveDiscontinuities[n] || (t == 0 && s.copyDiscontinuities[n]) {
		return true
	}
	if s.plan == nil || t != 0 || n <= 0 {
		return false
	}
	prev, cur := s.plan.ModeAt(n-1), s.plan.ModeAt(n)
	if prev == cur {
		return false
	}
	if prev == ModeFastStart && cur == ModeCopy {
		return s.rampTranscodesLocked()
	}
	return cur == ModeClamp || prev == ModeClamp
}

// runPlan is what one ffmpeg run starting at a segment must do.
type runPlan struct {
	decision  core.Decision
	hlsDur    time.Duration // -hls_time for this run
	fastStart bool
	mode      SegMode
	planEnd   int // exclusive; -1 = to the end (or unknown)
	rate      rateControl
}

// runPlanLocked resolves the plan at seg into a run: decision (primary
// decision, fast-start or clamp variant, or the wanted rung's), segment
// length for ffmpeg's muxer and the segment where the mode changes.
func (s *Session) runPlanLocked(seg int) runPlan {
	rp := runPlan{decision: s.trackDecisionLocked(s.wantTrack), hlsDur: s.segDur, planEnd: -1, rate: s.vbvRateLocked(s.wantTrack)}
	if s.copyFallback {
		// Keep the published geometry, but never return to the failed copy
		// mode on a seek, a governor boundary, or a rendition switch.
		rp.mode = ModeTranscode
		return rp
	}
	if s.isDASH() || s.plan == nil {
		return rp
	}

	if s.wantTrack != 0 {
		// The media playlist already commits every rendition to the base clock.
		// A lower rung must cut the same 2s startup slots before the 4s steady
		// slots, rather than labelling four seconds of media as a two-second slot.
		rp.mode = ModeTranscode
		if seg < s.plan.rampSegs {
			rp.hlsDur = s.plan.Dur(seg)
			rp.planEnd = s.plan.rampSegs
			rp.fastStart = true
		}
		return rp
	}
	rp.mode = s.plan.ModeAt(seg)
	rp.planEnd = s.plan.NextChange(seg)
	switch rp.mode {
	case ModeFastStart:
		rp.fastStart = true
		rp.hlsDur = s.plan.Dur(seg)
		if s.rampTranscodesLocked() {
			rp.decision = fastStartDecision(s.decision, s.info, s.adapt)
			rp.rate = rateControl{}
		}
	case ModeClamp:
		rp.decision = clampDecision(s.decision, s.info, s.plan.ClampKbps(seg))
		rp.rate = rateControl{}
	}
	// An in-session seek restart of a transcode (the playlist, and so the
	// segment lengths, are already fixed) still gets a fast start: the
	// first segments of the new run use the low-latency preset, then the
	// run chains into the steady preset. Copy sessions cannot do this
	// mid-playlist (the switch would need a discontinuity the player does
	// not have); they ramp when Plex restarts the session at the seek
	// offset, which is the usual seek path.
	if s.seekRampSeg == seg && s.adapt.FastStart && rp.mode == ModeTranscode && !rp.fastStart {
		rp.fastStart = true
		if end := seg + s.adapt.RampSegs; rp.planEnd < 0 || end < rp.planEnd {
			rp.planEnd = end
		}
	}
	return rp
}

// trackDecisionLocked is the steady decision of track t: an HLS rung's
// own decision, or for DASH the decision of the rung behind video
// track t.
func (s *Session) trackDecisionLocked(t int) core.Decision {
	if s.isDASH() {
		if r := rungForDashTrack(t); r >= 0 && r < len(s.rungs) {
			return s.rungs[r].decision
		}
		return s.decision
	}
	if t >= 0 && t < len(s.rungs) {
		return s.rungs[t].decision
	}
	return s.decision
}

// vbvRateLocked is track t's VBV rate plan for a new run. A DASH track is
// not a rung index (track 1 is audio); map it like trackDecisionLocked.
func (s *Session) vbvRateLocked(t int) rateControl {
	if s.isDASH() {
		t = rungForDashTrack(t)
	}
	if !s.adapt.VBV || t < 0 || t >= len(s.rungs) {
		return rateControl{}
	}
	r := s.rungs[t]
	if r.decision.VideoCopy || r.peakKbps <= 0 {
		return rateControl{}
	}
	maxrate := r.peakKbps
	if gp := s.goodput.Kbps(); gp > 0 {
		// Leave a modest delivery margin while allowing the reservoir to
		// spend bits on complex scenes.
		cap := gp * 9 / 10
		if cap > 0 && cap < maxrate {
			maxrate = cap
		}
	}
	if maxrate < 300 {
		maxrate = 300
	}
	seconds := s.adapt.VBVBufSeconds
	if seconds <= 0 {
		seconds = 8
	}
	return rateControl{Quality: true, MaxrateKbps: maxrate,
		BufsizeKbps: maxrate * seconds, TargetKbps: r.avgKbps}
}

// maybeGovernLocked re-plans upcoming copy segments as clamped transcodes
// when their bitrate spikes over the measured goodput (the clamp plan lives
// in Timeline.Clamp, segment_plan.go).
func (s *Session) maybeGovernLocked() {
	if !s.governorOn || s.plan == nil || s.goodput.Samples() < 2 || len(s.segSizes) == 0 {
		return
	}
	goodput := s.goodput.Kbps()
	if goodput <= 0 {
		return
	}
	// Only alter not-yet-requested segments in the encoder read-ahead
	// window. EVENT playlists let clients discover the revised plan.
	limit := s.aheadLimit()
	if limit <= 0 {
		limit = int(24 * time.Second / s.segDur)
	}
	maxSegs := limit
	if maxSegs < 2 {
		maxSegs = 2
	}
	margin := s.adapt.SpikeMargin
	if margin < 1 {
		margin = 1.25
	}
	for n := s.lastReq + 1; n <= s.lastReq+maxSegs; n++ {
		if s.plan.ModeAt(n) != ModeCopy || s.plan.ClampKbps(n) != 0 {
			continue
		}
		sz := s.segSizes[n]
		if sz <= 0 {
			continue
		}
		dur := s.plan.Dur(n)
		if dur <= 0 {
			dur = s.segDur
		}
		kbps := int(float64(sz*8) / dur.Seconds() / 1000)
		if float64(kbps) <= float64(goodput)*margin {
			continue
		}
		// Clamp total output below observed goodput; clampDecision retains
		// the source resolution and audio selection.
		clamp := goodput * 8 / 10
		if clamp < 300 {
			clamp = 300
		}
		s.plan.SetClamp(n, clamp)
		s.playlist = nil
		// Stop the active primary copy run at the new boundary. The run's
		// normal chaining path starts a clamp encode there; discard any copy
		// files already written at or beyond it so they cannot win a request
		// race before the replacement is complete.
		if r := s.cur; r != nil && r.track == 0 && r.mode == ModeCopy &&
			(r.planEnd < 0 || n < r.planEnd) {
			r.planEnd = n
			for produced := range s.done[0] {
				if produced >= n {
					delete(s.done[0], produced)
					_ = os.Remove(s.trackSegPath(0, produced))
				}
			}
		}
		s.log.Info("governed VBR spike", "segment", n, "sourceKbps", kbps,
			"goodputKbps", goodput, "clampKbps", clamp)
		break // one clamp run at a time; re-evaluate after it completes
	}
}
