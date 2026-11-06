package remotetranscode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func recoveryTransportRequest() core.HLSRequest {
	d := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", AudioCopy: true, Container: "mpegts", AudioStream: 1, SubtitleStream: -1}
	a := &core.RecoveryAACCursor{SourceRevision: "immutable-revision", StreamIndex: 1, SampleRate: 48000, Channels: 1, SamplesPerAU: 1024, NextSample: 184320, CodecConfigSHA256: [32]byte{1}, PreviousRawAUSHA256: [32]byte{2}, NextRawAUSHA256: [32]byte{3}, SourceNext: core.RecoveryPacketClock{PTS: 184320, DTS: 184320, TimeBaseNum: 1, TimeBaseDen: 48000}, OutputNext: core.RecoveryPacketClock{PTS: 9345600, DTS: 9345600, TimeBaseNum: 1, TimeBaseDen: 90000}}
	return core.HLSRequest{SessionID: "session", Protocol: core.ProtocolHLS, SegmentDuration: 1920 * time.Millisecond, Decision: d, Info: core.MediaInfo{Duration: 8 * time.Second, Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", FrameRate: 25, Width: 160, Height: 90}, {Index: 1, Type: "audio", Codec: "aac", Profile: "LC", Channels: 1, SampleRate: 48000}}}, RecoveryStart: &core.RecoveryStart{TitlePosition: 3840 * time.Millisecond, FirstSequence: 77, OutputTimestamp: 103840 * time.Millisecond, AdmittedDecision: d, Audio: a}}
}

func TestRecoveryStartNegotiationBeforeAssignment(t *testing.T) {
	r := recoveryTransportRequest()
	caps, err := NormalizeCapabilities(Capabilities{})
	if err != nil || caps.RecoveryStart || SupportsRequest(r, caps) {
		t.Fatal("recovery enabled by default")
	}
	assigned := false
	a := &Adapter{opts: AdapterOptions{Capabilities: caps, Assign: func(context.Context, core.HLSRequest) (Assignment, error) {
		assigned = true
		return Assignment{}, errors.New("assign")
	}}}
	_, _, err = a.StartHLSOwned(t.Context(), r)
	if !errors.Is(err, ErrUnsupported) || assigned {
		t.Fatal("unsupported request reached assignment")
	}
	caps.RecoveryStart = true
	if !SupportsRequest(r, caps) {
		t.Fatal("explicit candidate instruction rejected")
	}
	a.opts.Capabilities = caps
	a.opts.Assign = func(_ context.Context, r core.HLSRequest) (Assignment, error) {
		r.RecoveryStart.Audio.NextSample = 0
		return Assignment{}, errors.New("stop before network")
	}
	_, _, _ = a.StartHLSOwned(t.Context(), r)
	if r.RecoveryStart.Audio.NextSample != 184320 {
		t.Fatal("assignment callback mutated caller")
	}
	b, _ := json.Marshal(Start{})
	if strings.Contains(string(b), "RecoveryStart") {
		t.Fatal("nil changed Start wire")
	}
}

func TestRecoveryStartWorkerPreservesImmutableInstructions(t *testing.T) {
	w, f, s := fixture(t)
	r := recoveryTransportRequest()
	s.Info, s.Decision, s.Protocol, s.SegmentDuration, s.RecoveryStart = r.Info, r.Decision, r.Protocol, r.SegmentDuration, r.RecoveryStart
	if rpc(w, "start", s, true).Code != 422 || f.calls != 0 {
		t.Fatal("unnegotiated request invoked backend")
	}
	w.opts.Capabilities.RecoveryStart = true
	w.opts.Security.Authorize = func(_ context.Context, _ string, _ string, _ Identity, s *Start) error {
		if s != nil && s.RecoveryStart != nil {
			s.RecoveryStart.Audio.NextSample = 0
		}
		return nil
	}
	if state(t, rpc(w, "start", s, true)) != "running" {
		t.Fatal("instruction transport failed")
	}
	defer close(f.session.done)
	if !reflect.DeepEqual(f.request.RecoveryStart, s.RecoveryStart) {
		t.Fatal("lost cursor in backend reconstruction")
	}
	encoded, _ := json.Marshal(s)
	if w.records[s.Identity].hash != sha256.Sum256(encoded) {
		t.Fatal("cursor absent from Start hash")
	}
	f.request.RecoveryStart.Audio.NextSample = 0
	f.request.RecoveryStart.FirstSequence = 0
	if !reflect.DeepEqual(w.records[s.Identity].start.RecoveryStart, s.RecoveryStart) {
		t.Fatal("backend mutated immutable record")
	}
	if state(t, rpc(w, "start", s, true)) != "running" || f.calls != 1 {
		t.Fatal("duplicate invoked encoder")
	}
	for name, mutate := range map[string]func(*core.RecoveryStart){
		"source":      func(x *core.RecoveryStart) { x.Audio.SourceRevision = "different" },
		"next-au":     func(x *core.RecoveryStart) { x.Audio.NextRawAUSHA256[0]++ },
		"previous-au": func(x *core.RecoveryStart) { x.Audio.PreviousRawAUSHA256[0]++ },
		"sequence":    func(x *core.RecoveryStart) { x.FirstSequence++ },
		"epoch":       func(x *core.RecoveryStart) { x.SeekEpoch++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := s
			changed.RecoveryStart = core.CloneRecoveryStart(s.RecoveryStart)
			mutate(changed.RecoveryStart)
			if rpc(w, "start", changed, true).Code != 409 {
				t.Fatal("changed cursor replay accepted")
			}
		})
	}
}

// A policy rejection must see the complete cursor and precede source acquisition
// and immutable execution reservation, even when transport is explicitly enabled.
type recoveryRejectingBackend struct {
	fakeEncoder
	seen *core.RecoveryStart
}

func (f *recoveryRejectingBackend) SupportsDistributedRequest(r core.HLSRequest) bool {
	f.seen = core.CloneRecoveryStart(r.RecoveryStart)
	return false
}
func TestRecoveryStartBackendPreflightSeesCursorBeforeReservation(t *testing.T) {
	w, _, s := fixture(t)
	backend := &recoveryRejectingBackend{}
	w.opts.Encoder = backend
	w.opts.Capabilities.RecoveryStart = true
	resolved := false
	w.opts.Resolve = func(context.Context, string, Start) (Source, error) {
		resolved = true
		return Source{}, errors.New("must not resolve")
	}
	r := recoveryTransportRequest()
	s.Info, s.Decision, s.Protocol, s.SegmentDuration, s.RecoveryStart = r.Info, r.Decision, r.Protocol, r.SegmentDuration, r.RecoveryStart
	if rpc(w, "start", s, true).Code != 422 || resolved || backend.calls != 0 || len(w.records) != 0 || !reflect.DeepEqual(backend.seen, s.RecoveryStart) {
		t.Fatal("preflight lost cursor or acquired responsibility/source")
	}
}
