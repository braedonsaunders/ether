package sessions

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type activitySession struct {
	fakeMediaSession
	state string
}

func (s *activitySession) PlaybackState(state string) { s.state = state }

type activityTranscoder struct {
	fakeBrokerTranscoder
	output *activitySession
}

func (t activityTranscoder) Session(id string) (core.HLSSession, bool) {
	return t.output, id == t.output.id
}

func TestTimelineNotifiesTranscodeActivity(t *testing.T) {
	s := &activitySession{fakeMediaSession: fakeMediaSession{id: "session"}}
	tc := activityTranscoder{output: s}
	NotifyTranscodeActivity(tc, "session", "paused")
	if s.state != "paused" {
		t.Fatalf("state %q", s.state)
	}
	NotifyTranscodeActivity(tc, "other", "stopped")
	if s.state != "paused" {
		t.Fatal("unrelated output was notified")
	}
}
