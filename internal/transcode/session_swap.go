package transcode

import (
	"context"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"os"
	"strings"
	"time"
)

// SwapInput implements core.HLSSession: restart on a new input at `at`
// (mid-stream failover). Segments from `at` on are discarded and produced
// again from the new input; numbering stays continuous.
func (s *Session) SwapInput(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration) error {
	return s.swapInput(ctx, inputURL, info, at, false)
}

func (s *Session) SwapInputBuffered(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration) error {
	return s.swapInput(ctx, inputURL, info, at, true)
}

func (s *Session) swapInput(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration, preserve bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if inputURL == "" {
		return errors.New("transcode: SwapInput: empty input URL")
	}
	if err := s.CheckSwap(info); err != nil {
		return err
	}
	// Decode a tiny bounded sample outside the session lock. Only proven
	// embedded side bars matching the existing picture are removed.
	sampleAt := at
	if preserve {
		b := s.RecoveryBuffer()
		if b.ReadyUntil > sampleAt && b.ReadyUntil < info.Duration {
			sampleAt = b.ReadyUntil
		}
	}
	crop, err := s.swapPictureCrop(ctx, inputURL, info, sampleAt)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("transcode: session closed")
	}
	// The caller verifies identity alignment, including the buffered boundary.
	// Different trailing credits or container rounding need not discard media.
	preserve = preserve && info.Duration > 0
	if preserve {
		b := s.recoveryBufferLocked()
		// Preserve at least one whole published segment; never seek past EOF.
		preserve = b.ReadyUntil > b.Position && b.ReadyUntil < info.Duration
		if preserve {
			at = b.ReadyUntil
		}
	}
	seg := 0
	if at > s.base {
		if s.plan != nil && !s.isDASH() {
			seg = s.plan.IndexAt(at - s.base)
		} else {
			seg = int((at - s.base) / s.segDur)
		}
	}
	if s.total > 0 && seg >= s.total {
		seg = s.total - 1
	}
	s.stopRunForLocked("input swap")
	for t, done := range s.done {
		for n := range done {
			if n >= seg {
				_ = os.Remove(s.trackSegPath(t, n))
				delete(done, n)
			}
		}
	}
	next := adaptToContract(s.audioMatchBaseDecision(), s.info, info, s.container, s.contract)
	next = s.applyAudioMatchLocked(inputURL, next)
	if inputURL == s.input {
		next.AudioStream = s.decision.AudioStream
		next.SubtitleStream = s.decision.SubtitleStream
	}
	if next.Enhancement.Backend == rtxBackend && next.Enhancement.State == "stopped" && next.Enhancement.Width > 0 {
		next.Enhancement.State = "selected"
	}
	if s.isDASH() {
		next = dashDecision(info, next)
	}
	if next.Kind != s.decision.Kind || next.VideoCopy != s.decision.VideoCopy || next.AudioCopy != s.decision.AudioCopy {
		s.log.Info("failover changes decision", "from", s.decision.Kind, "to", next.Kind, "reason", next.Reason)
	}
	// Keep the ladder's track layout; adapt every rung like the primary.
	if len(s.rungs) > 0 {
		s.rungs[0].decision = next
		for i := 1; i < len(s.rungs); i++ {
			old := s.rungs[i].decision
			nd := adaptDecision(old, s.info, info, s.container)
			if s.isDASH() {
				nd = dashDecision(info, nd)
			}
			nd.VideoCopy = false
			nd.Kind = core.DecisionTranscode
			nd.MaxHeight, nd.BitrateKbps = old.MaxHeight, old.BitrateKbps
			s.rungs[i].decision = nd
		}
	}
	if s.input != inputURL {
		// SwapInput has no replacement-credential parameter. Never carry an
		// old provider's credentials onto an independently resolved URL.
		s.inputHeaders = nil
	}
	s.inputCrop = crop
	s.input = inputURL
	s.info = info
	s.decision = next
	s.endSeg = -1
	s.failures = 0
	s.failErr = nil
	s.inputFailed = false
	if !preserve {
		s.lastReq = seg
	}
	s.seekRampSeg = seg
	s.log.Info("input swapped", "at", at, "segment", seg)
	return s.startRunForLocked(seg, false, "input swap")
}

// CheckSwap reports whether SwapInput can deliver info's video in the
// format the client already opened. Playback calls it before committing
// a replacement so an incompatible candidate is skipped, not spliced.
func (s *Session) CheckSwap(info core.MediaInfo) error {
	if s.contract == nil {
		return nil
	}
	return s.contract.check(info, s.m.feat)
}

// adaptDecision keeps a failover's output compatible with what the client
// is already playing: same codecs, container and size. Streams are matched
// by language; anything the new release cannot copy is transcoded to the
// codec the client already accepted.
func adaptDecision(old core.Decision, oldInfo, newInfo core.MediaInfo, container string) core.Decision {
	d := old
	d.Container = container
	var notes []string

	// Audio: same language (and codec when possible) as before.
	oldA := findStream(oldInfo, old.AudioStream, "audio")
	newA := matchStream(newInfo, oldA, "audio")
	d.AudioStream = -1
	if newA != nil {
		d.AudioStream = newA.Index
	}
	// Subtitles.
	oldS := findStream(oldInfo, old.SubtitleStream, "subtitle")
	newS := matchStream(newInfo, oldS, "subtitle")
	d.SubtitleStream = -1
	if oldS != nil && newS != nil {
		d.SubtitleStream = newS.Index
	}
	if d.BurnSubtitles && newS == nil {
		d.BurnSubtitles = false
		notes = append(notes, "subtitle track missing in new release")
	}
	if newS != nil && IsImageSubtitle(newS.Codec) && oldS != nil {
		d.BurnSubtitles = true
	}

	oldV, newV := oldInfo.Video(), newInfo.Video()
	if newV != nil && oldV != nil {
		oldRange := dynamicRange(oldV)
		if old.StripDolbyVision {
			oldRange = rangeHDR10 // the decoder holds HDR10, not source DV headers
		}
		if d.VideoCopy {
			sameRange := oldRange == dynamicRange(newV) ||
				(dynamicRange(newV) == rangeDV && rangeOfDVBase(newV) == oldRange)
			if newV.Codec != oldV.Codec || newV.Height > oldV.Height || !sameRange || d.BurnSubtitles {
				d.Kind = core.DecisionTranscode
				d.VideoCopy = false
				d.VideoCodec = oldV.Codec
				if d.VideoCodec != "h264" && d.VideoCodec != "hevc" {
					d.VideoCodec = "h264"
				}
				d.MaxHeight = oldV.Height
				if d.BitrateKbps <= 0 {
					d.BitrateKbps = bitrateCeiling(d.MaxHeight, d.VideoCodec)
				}
				d.ToneMap = encodedRange(newV) != rangeSDR && !isHDR(oldV)
				notes = append(notes, fmt.Sprintf("video %s %dp re-encoded to match the stream", newV.Codec, newV.Height))
			}
		} else {
			keptHDR := isHDR(oldV) && !old.ToneMap
			d.ToneMap = encodedRange(newV) != rangeSDR && !keptHDR
		}
		d.StripDolbyVision = d.VideoCopy && oldRange == rangeHDR10 && newV.DolbyVision && rangeOfDVBase(newV) == rangeHDR10
	}
	var audioNote string
	d, audioNote = preserveAudio(d, oldA, newA)
	if audioNote != "" {
		notes = append(notes, audioNote)
	}
	if !d.AudioCopy && newA != nil {
		switch d.AudioCodec {
		case "aac", "ac3", "eac3", "mp3":
		case "opus":
			if container != ContainerFMP4 {
				d.AudioCodec = "aac"
			}
		default: // not encodable for HLS (flac, truehd, dts...)
			d.AudioCodec = "aac"
		}
	}
	if d.Kind == core.DecisionDirectPlay {
		d.Kind = core.DecisionRemux
	}
	if len(notes) > 0 {
		d.Reason = old.Reason + "; failover: " + strings.Join(notes, ", ")
	}
	return d
}

func rangeOfDVBase(v *core.StreamInfo) string {
	b, ok := dvBaseLayer(v)
	if !ok {
		return ""
	}
	return b
}

// matchStream finds the stream in info that best matches prev: same
// language and codec, then same language, then the default/first one.
func matchStream(info core.MediaInfo, prev *core.StreamInfo, typ string) *core.StreamInfo {
	var sameLang, first, def *core.StreamInfo
	for i := range info.Streams {
		s := &info.Streams[i]
		if s.Type != typ || s.External {
			continue
		}
		if first == nil {
			first = s
		}
		if s.Default && def == nil {
			def = s
		}
		if prev == nil {
			continue
		}
		if s.Language == prev.Language {
			if s.Codec == prev.Codec && s.Forced == prev.Forced {
				return s
			}
			if sameLang == nil {
				sameLang = s
			}
		}
	}
	if sameLang != nil {
		return sameLang
	}
	if typ == "subtitle" {
		return nil // never invent subtitles the user did not pick
	}
	if def != nil {
		return def
	}
	return first
}
