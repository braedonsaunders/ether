package transcode

import (
	"errors"
	"math/big"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var ErrRecoveryUnsupported = errors.New("transcode: verified recovery source/cursor provider unavailable")

// ValidRecoveryStartShape checks only the bounded candidate instruction/profile.
// It performs no I/O and proves none of the expected facts. In particular true
// does not imply Manager support: an authenticated immutable source/AU index,
// actual final-byte validation and client acceptance are still required.
func ValidRecoveryStartShape(r core.HLSRequest) bool {
	x := r.RecoveryStart
	if x == nil {
		return true
	}
	if r.StartOffset != 0 || x.TitlePosition < 0 || x.FirstSequence < 0 || x.FirstSequence > 10000000 || x.SeekEpoch < 0 || x.OutputTimestamp < 0 ||
		(r.Protocol != "" && r.Protocol != core.ProtocolHLS) || r.LiveInput || r.CopyTimestamps || len(r.SlateText) > 0 || r.SoftSubtitleStream != nil ||
		r.SegmentDuration != 1920*time.Millisecond || r.Info.Estimated || r.Info.Duration <= x.TitlePosition || x.Audio == nil {
		return false
	}
	if !recoveryCopyPlan(r.Decision) || !recoveryCopyPlan(x.AdmittedDecision) {
		return false
	}
	a, d, old := x.Audio, r.Decision, x.AdmittedDecision
	if a.SourceRevision == "" || len(a.SourceRevision) > 512 || a.StreamIndex < 0 || a.StreamIndex > 100000 ||
		a.SampleRate != 48000 || (a.Channels != 1 && a.Channels != 2) || a.SamplesPerAU != 1024 || a.NextSample < 0 || a.NextSample%1024 != 0 ||
		a.CodecConfigSHA256 == ([32]byte{}) || a.PreviousRawAUSHA256 == ([32]byte{}) || a.NextRawAUSHA256 == ([32]byte{}) ||
		d.AudioStream != a.StreamIndex || old.AudioStream != a.StreamIndex ||
		d.MaxHeight != old.MaxHeight || d.H264Level != old.H264Level || d.H264FrameRate != old.H264FrameRate || d.BitrateKbps != old.BitrateKbps ||
		d.AudioChannels != old.AudioChannels || d.AudioSampleRate != old.AudioSampleRate ||
		(d.AudioChannels != 0 && d.AudioChannels != a.Channels) || (d.AudioSampleRate != 0 && d.AudioSampleRate != a.SampleRate) {
		return false
	}
	v, src := r.Info.Video(), findStream(r.Info, a.StreamIndex, "audio")
	if v == nil || v.VariableFrameRate || v.FrameRate != 25 || src == nil || src.Codec != "aac" || src.Profile != "LC" || src.AACProgramConfig ||
		src.SampleRate != a.SampleRate || src.Channels != a.Channels || x.TitlePosition%(40*time.Millisecond) != 0 {
		return false
	}
	const wrap = time.Duration((int64(1) << 33) * int64(time.Second) / 90000)
	if x.OutputTimestamp >= wrap || r.Info.Duration-x.TitlePosition >= wrap-x.OutputTimestamp {
		return false
	}
	span := r.Info.Duration - x.TitlePosition
	if int64(span/r.SegmentDuration)+1 > 10000000-x.FirstSequence {
		return false
	}
	return recoveryClockEquals(a.SourceNext, int64(x.TitlePosition), int64(time.Second)) &&
		recoveryClockEquals(a.SourceNext, a.NextSample, int64(a.SampleRate)) &&
		recoveryClockEquals(a.OutputNext, int64(x.OutputTimestamp), int64(time.Second))
}

func recoveryCopyPlan(d core.Decision) bool {
	return d.Kind == core.DecisionTranscode && !d.VideoCopy && d.AudioCopy && d.VideoCodec == "h264" && d.AudioCodec == "aac" &&
		d.Container == ContainerMPEGTS && !d.BurnSubtitles && d.SubtitleStream < 0 && len(d.AudioFilters) == 0 &&
		!enhancementSelected(d) && !d.ToneMap && !d.StripDolbyVision && (d.H264FrameRate == 0 || d.H264FrameRate == 25)
}

func recoveryClockEquals(c core.RecoveryPacketClock, numerator, denominator int64) bool {
	if c.PTS < 0 || c.DTS != c.PTS || c.TimeBaseNum <= 0 || c.TimeBaseNum > 1000000000 || c.TimeBaseDen <= 0 || c.TimeBaseDen > 1000000000 || numerator < 0 || denominator <= 0 {
		return false
	}
	// Bounded int64 inputs; arbitrary precision prevents overflow in products.
	lhs := new(big.Int).Mul(big.NewInt(c.PTS), big.NewInt(c.TimeBaseNum))
	lhs.Mul(lhs, big.NewInt(denominator))
	rhs := new(big.Int).Mul(big.NewInt(numerator), big.NewInt(c.TimeBaseDen))
	return lhs.Cmp(rhs) == 0
}
