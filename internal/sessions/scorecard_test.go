//go:build scorecard && !plexparity

package sessions

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type scorecardTestSession struct{ fakeMediaSession }

func (*scorecardTestSession) Decision() core.Decision {
	return core.Decision{Kind: core.DecisionTranscode, VideoCopy: false, AudioCopy: true}
}

type scorecardTestTranscoder struct {
	fakeBrokerTranscoder
	output *scorecardTestSession
}

func (t scorecardTestTranscoder) Session(id string) (core.HLSSession, bool) {
	return t.output, id == t.output.id
}

func TestScorecardSessionsFindsEncoderByPlaybackID(t *testing.T) {
	table := NewBindingTable[frontend.PlayIntent]()
	table.Put(&Binding[frontend.PlayIntent]{ID: "broker-session", PlayID: "source-playback", Started: true})
	output := &scorecardTestSession{fakeMediaSession: fakeMediaSession{id: "broker-session"}}
	b := &Broker{deps: BrokerDeps{Transcoder: scorecardTestTranscoder{output: output}}, bindings: table,
		sessions: map[string]*brokerSession{"broker-session": {playID: "source-playback"}}}
	got := b.ScorecardSessions(context.Background())
	if len(got) != 1 || !got[0].TranscoderActive || got[0].TranscoderDecision == nil || got[0].TranscoderDecision.VideoCopy || !got[0].TranscoderDecision.AudioCopy {
		t.Fatalf("encoder not correlated: %+v", got)
	}
}
