package sessions

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type decisionTranscoder struct{ decision core.Decision }

func (f decisionTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	panic("unexpected probe")
}
func (f decisionTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return f.decision
}
func (f decisionTranscoder) StartHLS(context.Context, core.HLSRequest) (core.HLSSession, error) {
	panic("unexpected start")
}
func (f decisionTranscoder) Session(string) (core.HLSSession, bool) { panic("unexpected session") }
func (f decisionTranscoder) Stop(string)                            { panic("unexpected stop") }
func (f decisionTranscoder) HWAccel() core.HWAccel                  { panic("unexpected hw") }
func TestDecisionRemuxOverride(t *testing.T) {
	ps := &core.PlaySession{Resolution: &core.Resolution{Release: core.Release{ID: "usenet:1"}}}
	tc := decisionTranscoder{decision: core.Decision{Kind: core.DecisionDirectPlay}}
	r := DecisionRequest{Session: ps, PreferRemux: true, Options: core.DecideOptions{DirectStream: true}, Protocol: core.ProtocolDASH}
	got, err := Decide(nil, tc, r)
	if err != nil || got.Kind != core.DecisionRemux || got.Container != "fmp4" || !got.VideoCopy || !got.AudioCopy {
		t.Fatalf("decision %+v, %v", got, err)
	}
	ps.Resolution.Release.Protocol = core.ProtocolTorrent
	got, err = Decide(nil, tc, r)
	if err != nil || got.Kind != core.DecisionDirectPlay {
		t.Fatalf("torrent decision %+v, %v", got, err)
	}
	ps.RequiresRemux = true
	got, err = Decide(nil, tc, r)
	if err != nil || got.Kind != core.DecisionRemux {
		t.Fatalf("required remux %+v, %v", got, err)
	}
}

func TestHLSDecisionReconsidersClientRejection(t *testing.T) {
	called := false
	got := HLSDecision(core.Decision{Kind: core.DecisionDirectPlay}, false, func() core.Decision { called = true; return core.Decision{Kind: core.DecisionTranscode} }, nil)
	if !called || got.Kind != core.DecisionTranscode || got.Container != "mpegts" {
		t.Fatalf("decision %+v, reconsider=%v", got, called)
	}
	got = HLSDecision(core.Decision{Kind: core.DecisionDirectPlay}, true, func() core.Decision { t.Fatal("unexpected reconsider"); return core.Decision{} }, nil)
	if got.Kind != core.DecisionRemux || !got.VideoCopy || !got.AudioCopy || got.Container != "mpegts" {
		t.Fatalf("decision %+v", got)
	}
}

func TestHLSDecisionForcedTargetBranch(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		initial                    core.DecisionKind
		allowDirect                bool
		reconsidered               core.DecisionKind
		wantReconsider, wantForced bool
	}{
		{"accepted direct still needs segmented output", core.DecisionDirectPlay, true, core.DecisionDirectPlay, false, true},
		{"rejected direct becomes transcode", core.DecisionDirectPlay, false, core.DecisionTranscode, true, false},
		{"rejected direct remains direct", core.DecisionDirectPlay, false, core.DecisionDirectPlay, true, true},
		{"existing remux", core.DecisionRemux, true, core.DecisionRemux, false, false},
		{"existing transcode", core.DecisionTranscode, false, core.DecisionTranscode, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reconsidered, forced := false, false
			got := HLSDecision(core.Decision{Kind: tc.initial, AudioCodec: "eac3"}, tc.allowDirect, func() core.Decision {
				reconsidered = true
				return core.Decision{Kind: tc.reconsidered, AudioCodec: "eac3"}
			}, func(dec core.Decision) core.Decision {
				forced = true
				if dec.Kind != core.DecisionRemux || !dec.VideoCopy || !dec.AudioCopy || dec.Container != "mpegts" {
					t.Fatalf("output target ran before forced-remux facts: %+v", dec)
				}
				return core.Decision{Kind: core.DecisionTranscode, VideoCopy: true, AudioCodec: "aac"}
			})
			if reconsidered != tc.wantReconsider || forced != tc.wantForced {
				t.Fatalf("reconsider=%v forced=%v", reconsidered, forced)
			}
			if got.Container != "mpegts" {
				t.Fatalf("missing post-reconsider container default: %+v", got)
			}
			if forced && (got.AudioCodec != "aac" || got.AudioCopy || !got.VideoCopy) {
				t.Fatalf("target decision overwritten: %+v", got)
			}
		})
	}
}

type targetDecisionTranscoder struct {
	decisionTranscoder
	options []core.DecideOptions
}

func (f *targetDecisionTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	f.options = append(f.options, opts)
	if opts.DirectPlay {
		return core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, AudioCodec: "eac3", AudioStream: 7}
	}
	return core.Decision{Kind: core.DecisionTranscode, VideoCopy: true, AudioCodec: "aac", AudioStream: 7}
}

func TestDecisionForcedRemuxHonorsAudioTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		target    []string
		prefer    bool
		wantCalls int
		wantKind  core.DecisionKind
	}{
		{"AAC target on forced stream", []string{"aac"}, true, 2, core.DecisionTranscode},
		{"surround target on forced stream", []string{"aac", "eac3"}, true, 1, core.DecisionRemux},
		{"no target", nil, true, 1, core.DecisionRemux},
		{"direct play exemption", []string{"aac"}, false, 1, core.DecisionDirectPlay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := &core.PlaySession{Resolution: &core.Resolution{Release: core.Release{ID: "usenet:fixture"}}, Info: core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Index: 7, Codec: "eac3"}}}}
			transcoder := &targetDecisionTranscoder{}
			got, err := Decide(nil, transcoder, DecisionRequest{Session: ps, Caps: core.ClientCaps{TranscodeAudioCodecs: tc.target}, Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: 7}, PreferRemux: tc.prefer, Protocol: core.ProtocolDASH})
			if err != nil || got.Kind != tc.wantKind || len(transcoder.options) != tc.wantCalls {
				t.Fatalf("decision %+v error %v calls %v", got, err, transcoder.options)
			}
			if tc.wantCalls == 2 && (transcoder.options[1].DirectPlay || !transcoder.options[1].DirectStream || transcoder.options[1].AudioStream != 7 || got.AudioCodec != "aac" || !got.VideoCopy) {
				t.Fatalf("target reconsider lost selected source facts: %+v %+v", got, transcoder.options)
			}
		})
	}
}
