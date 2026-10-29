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
	got := HLSDecision(core.Decision{Kind: core.DecisionDirectPlay}, false, func() core.Decision { called = true; return core.Decision{Kind: core.DecisionTranscode} })
	if !called || got.Kind != core.DecisionTranscode || got.Container != "mpegts" {
		t.Fatalf("decision %+v, reconsider=%v", got, called)
	}
	got = HLSDecision(core.Decision{Kind: core.DecisionDirectPlay}, true, func() core.Decision { t.Fatal("unexpected reconsider"); return core.Decision{} })
	if got.Kind != core.DecisionRemux || !got.VideoCopy || !got.AudioCopy || got.Container != "mpegts" {
		t.Fatalf("decision %+v", got)
	}
}
