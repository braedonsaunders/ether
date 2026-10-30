package transcode

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// The ABR ladder: one session, one shared segment Timeline, several
// rungs. The primary rung ("base", track 0) is the decision the session
// was started with; lower rungs are transcodes planned from the same
// decision. Only one ffmpeg runs at a time — a segment request on an
// idle rung restarts the encoder at that segment with the rung's
// decision (unused rungs are therefore stopped, not just paused, which
// is the cheapest idle state). Segment files are kept per rung (the
// primary in the session dir, rungs in rung-<id>/ subdirs) because the
// same segment number has different bytes per rung.
//
// DASH uses the same rungs: representations 0 (primary video) and 1
// (audio) keep their historic numbering, extra video representations
// are numbered after them. The DASH muxer still produces one video +
// one audio stream per run; the run's video chunks are regrouped into
// whichever video representation the client asked for.

// rung is one ladder variant.
type rung struct {
	id         string
	decision   core.Decision
	width      int
	height     int
	avgKbps    int // average total bitrate
	peakKbps   int
	codecs     string // RFC 6381, video,audio
	videoCodec string
}

// ladderSteps are the candidate rungs below the source (id, height,
// total kbps).
var ladderSteps = []struct {
	id     string
	height int
	kbps   int
}{
	{"1080p8m", 1080, 8000},
	{"720p4m", 720, 4000},
	{"480p1500k", 480, 1500},
	{"360p800k", 360, 800},
}

// rungCodecs builds the codecs string of an encode/copy decision.
func rungCodecs(info core.MediaInfo, d core.Decision, outHeight int) string {
	v := info.Video()
	if v == nil {
		return ""
	}
	codec := v.Codec
	profile, depth := v.Profile, v.BitDepth
	if !d.VideoCopy {
		codec = d.VideoCodec
		if codec == "" {
			codec = "h264"
		}
		profile, depth = "", 8
		if codec == "hevc" && keepsHDR(v) && !d.ToneMap {
			depth = 10
		}
	}
	c := estimateCodecs(codec, profile, outHeight, depth)
	if !d.VideoCopy && codec == "h264" && d.H264Level > 0 {
		c = fmt.Sprintf("avc1.6400%02x", d.H264Level)
	}
	if enhancementSelected(d) {
		// Match the native builder's explicit High 4.1/5.1 contract rather
		// than the ordinary encoder's resolution-only level estimate.
		c = "avc1.640029"
		if d.Enhancement.Width*d.Enhancement.Height > 1920*1088 || v.FrameRate > 30 {
			c = "avc1.640033"
		}
	}
	if a := findStream(info, d.AudioStream, "audio"); a != nil {
		ac := a.Codec
		ap := a.Profile
		if !d.AudioCopy {
			ac = d.AudioCodec
			if ac == "" {
				ac = "aac"
			}
			ap = ""
		}
		if s := estimateCodecs(ac, ap, 0, 0); s != "" {
			c += "," + s
		}
	}
	return c
}

// decisionTotalKbps is a decision's total (video+audio) average bitrate:
// what the stream carries, not the source file (which may hold more audio
// tracks, or audio the decision re-encodes).
func decisionTotalKbps(info core.MediaInfo, d core.Decision) int {
	audio := decisionAudioKbps(info, d)
	v := info.Video()
	if d.VideoCopy {
		video := sourceVideoKbps(info, v, nil)
		if video <= 0 && v != nil {
			video = bitrateCeiling(v.Height, v.Codec)
		}
		return video + audio
	}
	if d.BitrateKbps > 0 {
		return d.BitrateKbps
	}
	return videoBitrate(info, d) + audio
}

// decisionAudioKbps is the bitrate of a decision's audio output.
func decisionAudioKbps(info core.MediaInfo, d core.Decision) int {
	a := findStream(info, d.AudioStream, "audio")
	switch {
	case a == nil:
		return 0
	case d.AudioCopy:
		return copiedAudioKbps(a)
	default:
		return audioBitrateKbps(d.AudioCodec, outputChannels(d.AudioCodec, a))
	}
}

// minRungVideoKbps is the floor videoBitrate gives an encode's video.
const minRungVideoKbps = 200

// buildRungs plans a session's ladder from its primary decision. Rungs at
// or above the primary's output height or average bitrate are omitted, so
// a client bitrate cap (already applied to the primary decision by
// Decide) also caps the ladder.
func buildRungs(info core.MediaInfo, primary core.Decision, adapt adaptiveSettings) []rung {
	v := info.Video()
	pw, ph := 0, 0
	codec := ""
	if v != nil {
		codec = primary.VideoCodec
		if primary.VideoCopy {
			pw, ph, codec = v.Width, v.Height, v.Codec
		} else {
			pw, ph = decisionOutputSize(v, primary)
			if codec == "" {
				codec = "h264"
			}
		}
	}
	avg := decisionTotalKbps(info, primary)
	base := rung{
		id: "base", decision: primary, width: pw, height: ph,
		avgKbps: avg, videoCodec: codec,
		codecs: rungCodecs(info, primary, ph),
	}
	base.peakKbps = peakKbps(avg)
	rungs := []rung{base}
	if !adapt.Ladder || v == nil {
		return rungs
	}
	audio := decisionAudioKbps(info, primary) // rungs carry the primary's audio
	below := avg
	for _, step := range ladderSteps {
		if ph > 0 && step.height >= ph {
			continue
		}
		// Advertise what the rung really sends: its video never drops
		// below the encoder floor, so copied lossless or surround audio can
		// make a "lower" rung no cheaper than the one above it.
		total := max(step.kbps, minRungVideoKbps+audio)
		if below > 0 && total >= below {
			continue
		}
		d := primary
		// Enhancement belongs only to the base. These are ordinary encodes
		// of the source, including when the base upscales beyond the source.
		d.Enhancement = core.VideoEnhancement{}
		d.Kind = core.DecisionTranscode
		d.VideoCopy = false
		d.MaxHeight = minPositive(step.height, v.Height)
		d.BitrateKbps = total
		// Keep HDR only when the primary keeps it in HEVC; otherwise the
		// cheap, universally decoded SDR H.264 encode.
		d.VideoCodec = "h264"
		d.ToneMap = primary.ToneMap || encodedRange(v) != rangeSDR
		if keepsHDR(v) && !primary.ToneMap && (primary.VideoCodec == "hevc" || (primary.VideoCopy && v.Codec == "hevc")) {
			d.VideoCodec = "hevc"
			d.ToneMap = false
		}
		d.Reason = primary.Reason + "; ABR rung " + step.id
		if enhancementSelected(primary) {
			d.Reason = "ordinary ABR rung " + step.id + " beneath RTX Video base"
		}
		w, h := outputSize(v.Width, v.Height, step.height)
		rungs = append(rungs, rung{
			id: step.id, decision: d, width: w, height: h,
			avgKbps: total, peakKbps: peakKbps(total),
			videoCodec: d.VideoCodec, codecs: rungCodecs(info, d, h),
		})
		below = total
	}
	return rungs
}

func peakKbps(avg int) int {
	if avg <= 0 {
		return 0
	}
	p := avg * 12 / 10
	if p < avg+100 {
		p = avg + 100
	}
	return p
}

// rungTrack is the session track of a rung ID (-1 unknown). For DASH the
// track layout differs (see dashTrackForRung).
func (s *Session) rungTrack(id string) int {
	for i, r := range s.rungs {
		if r.id == id {
			if s.isDASH() {
				return dashTrackForRung(i)
			}
			return i
		}
	}
	return -1
}

// dashTrackForRung maps a rung index to its DASH session track: rung 0 is
// track 0 (video), track 1 is audio, further rungs follow.
func dashTrackForRung(rungIdx int) int {
	if rungIdx <= 0 {
		return 0
	}
	return rungIdx + 1
}

// rungForDashTrack is the inverse of dashTrackForRung for video tracks.
func rungForDashTrack(track int) int {
	if track <= 0 {
		return 0
	}
	return track - 1
}

// audioTrackLocked is the DASH audio track index (-1 when there is none
// or the session is HLS).
func (s *Session) audioTrackLocked() int {
	if !s.isDASH() || s.dash == nil {
		return -1
	}
	for i, r := range s.dash.reps {
		if r.Kind == "audio" {
			return i
		}
	}
	return -1
}

// Rungs implements core.HLSLadderSession.
func (s *Session) Rungs() []core.HLSRung {
	s.mu.Lock()
	defer s.mu.Unlock()
	fr := 0.0
	if v := s.info.Video(); v != nil {
		fr = v.FrameRate
	}
	out := make([]core.HLSRung, 0, len(s.rungs))
	for _, r := range s.rungs {
		out = append(out, core.HLSRung{
			ID: r.id, Bandwidth: r.peakKbps * 1000, AverageBandwidth: r.avgKbps * 1000,
			Width: r.width, Height: r.height, Codecs: r.codecs, FrameRate: fr,
		})
	}
	return out
}

// ServeRungPlaylist implements core.HLSLadderSession.
func (s *Session) ServeRungPlaylist(w http.ResponseWriter, r *http.Request, rungID string) {
	if s.isDASH() {
		s.ServeManifest(w, r)
		return
	}
	s.mu.Lock()
	t := s.rungTrack(rungID)
	if t < 0 || s.closed {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	body := s.renderPlaylistLocked(t)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// ServeRungSegment implements core.HLSLadderSession.
func (s *Session) ServeRungSegment(w http.ResponseWriter, r *http.Request, rungID string, n int) {
	s.mu.Lock()
	t := s.rungTrack(rungID)
	s.mu.Unlock()
	if t < 0 {
		http.NotFound(w, r)
		return
	}
	if n == InitSegment {
		s.serveInit(w, r, t)
		return
	}
	s.serveSegment(w, r, t, n)
}

// dashRepsLadder plans a DASH session's representations: primary video
// (track 0), audio (track 1), then one video representation per extra
// rung, all $Number$-aligned on the session's segment duration.
func dashRepsLadder(info core.MediaInfo, primary core.Decision, rungs []rung) []dashRep {
	reps := dashReps(info, primary)
	if enhancementSelected(primary) && len(rungs) > 0 {
		for i := range reps {
			if reps[i].Kind == "video" {
				reps[i].Codecs = strings.SplitN(rungs[0].codecs, ",", 2)[0]
			}
		}
	}
	for i := 1; i < len(rungs); i++ {
		sub := dashReps(info, rungs[i].decision)
		for _, r := range sub {
			if r.Kind != "video" {
				continue
			}
			r.ID = dashTrackForRung(i)
			reps = append(reps, r)
		}
	}
	return reps
}

var _ core.HLSLadderSession = (*Session)(nil)
var _ core.GoodputSession = (*Session)(nil)
