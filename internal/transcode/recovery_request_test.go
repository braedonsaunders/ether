package transcode

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func recoveryRequestFixture() core.HLSRequest {
	d := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", AudioCopy: true, Container: ContainerMPEGTS, AudioStream: 1, SubtitleStream: -1}
	a := &core.RecoveryAACCursor{SourceRevision: "immutable-revision", StreamIndex: 1, SampleRate: 48000, Channels: 1, SamplesPerAU: 1024, NextSample: 184320, CodecConfigSHA256: [32]byte{1}, PreviousRawAUSHA256: [32]byte{2}, NextRawAUSHA256: [32]byte{3}, SourceNext: core.RecoveryPacketClock{PTS: 184320, DTS: 184320, TimeBaseNum: 1, TimeBaseDen: 48000}, OutputNext: core.RecoveryPacketClock{PTS: 9345600, DTS: 9345600, TimeBaseNum: 1, TimeBaseDen: 90000}}
	return core.HLSRequest{SessionID: "recovery", InputURL: "http://127.0.0.1:1/source", Protocol: core.ProtocolHLS, SegmentDuration: 1920 * time.Millisecond, Decision: d, Info: core.MediaInfo{Duration: 8 * time.Second, Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", FrameRate: 25, Width: 160, Height: 90}, {Index: 1, Type: "audio", Codec: "aac", Profile: "LC", Channels: 1, SampleRate: 48000}}}, RecoveryStart: &core.RecoveryStart{TitlePosition: 3840 * time.Millisecond, FirstSequence: 77, OutputTimestamp: 103840 * time.Millisecond, SeekEpoch: 0, AdmittedDecision: d, Audio: a}}
}

func TestRecoveryStartShapeRejectsUnverifiedMappings(t *testing.T) {
	if !ValidRecoveryStartShape(recoveryRequestFixture()) {
		t.Fatal("candidate shape rejected")
	}
	cases := map[string]func(*core.HLSRequest){
		"no-audio":                  func(r *core.HLSRequest) { r.RecoveryStart.Audio = nil },
		"legacy-offset":             func(r *core.HLSRequest) { r.StartOffset = time.Second },
		"ordinary-four-second-grid": func(r *core.HLSRequest) { r.SegmentDuration = 4 * time.Second },
		"encoded-original":          func(r *core.HLSRequest) { r.RecoveryStart.AdmittedDecision.AudioCopy = false },
		"encoded-replacement":       func(r *core.HLSRequest) { r.Decision.AudioCopy = false },
		"changed-video-plan":        func(r *core.HLSRequest) { r.Decision.MaxHeight = 720 },
		"gap":                       func(r *core.HLSRequest) { r.RecoveryStart.Audio.OutputNext.PTS++ },
		"synthetic-sample":          func(r *core.HLSRequest) { r.RecoveryStart.Audio.NextSample += 1024 },
		"missing-predecessor":       func(r *core.HLSRequest) { r.RecoveryStart.Audio.PreviousRawAUSHA256 = [32]byte{} },
		"no-revision":               func(r *core.HLSRequest) { r.RecoveryStart.Audio.SourceRevision = "" },
		"fractional-title":          func(r *core.HLSRequest) { r.RecoveryStart.TitlePosition++ },
		"overflow-clock": func(r *core.HLSRequest) {
			r.RecoveryStart.Audio.SourceNext.PTS = math.MaxInt64
			r.RecoveryStart.Audio.SourceNext.DTS = math.MaxInt64
		},
		"bad-rational":      func(r *core.HLSRequest) { r.RecoveryStart.Audio.SourceNext.TimeBaseDen = 0 },
		"sequence-overflow": func(r *core.HLSRequest) { r.RecoveryStart.FirstSequence = math.MaxInt64 },
		"epoch":             func(r *core.HLSRequest) { r.RecoveryStart.SeekEpoch = -1 },
		"clock-wrap":        func(r *core.HLSRequest) { r.RecoveryStart.OutputTimestamp = 27 * time.Hour },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := recoveryRequestFixture()
			mutate(&r)
			if ValidRecoveryStartShape(r) {
				t.Fatal("unsupported instruction accepted")
			}
		})
	}
}

func TestRecoveryStartMissingProviderNeverInvokesEncoder(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	r := recoveryRequestFixture()
	if m.SupportsDistributedRequest(r) {
		t.Fatal("missing provider advertised support")
	}
	before := fr.startCount()
	s, retirement, err := m.StartHLSOwned(t.Context(), r)
	if s != nil || retirement == nil || !errors.Is(err, ErrRecoveryUnsupported) {
		t.Fatalf("got %v %v %v", s, retirement, err)
	}
	encoderSignal(t, retirement.EncoderDone())
	if retirement.EncoderErr() != nil || fr.startCount() != before {
		t.Fatal("unsupported run started or leaked responsibility")
	}
}
