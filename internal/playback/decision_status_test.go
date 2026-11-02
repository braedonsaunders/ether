package playback

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Sessions() must report the delivery that is actually running: a direct
// byte read first, then the broker-started HLS remux that replaces it
// (the production "Direct play" / "Starting" mix-up). Starting the remux
// also satisfies a pending remux requirement.
func TestSessionsReportCurrentDecision(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 200000, 100*time.Second)}, []string{"A"})
	ps := h.prepare(owner, movieKey, "c1")

	only := func() *core.PlaySession {
		t.Helper()
		got := h.m.Sessions()
		if len(got) != 1 {
			t.Fatalf("Sessions() = %d sessions, want 1", len(got))
		}
		return got[0]
	}
	if got := only(); got.Decision != "" {
		t.Fatalf("fresh session decision = %q, want unknown", got.Decision)
	}

	// A direct read marks direct play.
	srv := directPlayServer(t, h.m, ps.ID)
	if _, _, err := get(t, srv.URL, map[string]string{"Range": "bytes=0-1023"}); err != nil {
		t.Fatal(err)
	}
	if got := only(); got.Decision != core.DecisionDirectPlay || !got.DecisionVideoCopy || !got.DecisionAudioCopy {
		t.Fatalf("after direct read: %+v, want directplay copying both streams", got)
	}

	// The broker starts an HLS remux (video copied, audio transcoded)
	// for the same session: the snapshot must follow, and a pending
	// remux requirement is satisfied.
	s := h.m.lookup(ps.ID)
	s.mu.Lock()
	s.requiresRemux = true
	s.mu.Unlock()
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: false, Reason: "audio transcoded"})
	got := only()
	if got.Decision != core.DecisionRemux || !got.DecisionVideoCopy || got.DecisionAudioCopy {
		t.Fatalf("after HLS remux: decision=%q videoCopy=%v audioCopy=%v, want copy/true/false", got.Decision, got.DecisionVideoCopy, got.DecisionAudioCopy)
	}
	if got.RequiresRemux {
		t.Fatal("remux decision did not clear RequiresRemux in the snapshot")
	}
	if h.m.RequiresRemux(ps.ID) {
		t.Fatal("remux decision did not clear RequiresRemux")
	}

	// A later part probe must not flip the running encoder back to
	// direct play.
	if _, _, err := get(t, srv.URL, map[string]string{"Range": "bytes=0-1023"}); err != nil {
		t.Fatal(err)
	}
	if got := only(); got.Decision != core.DecisionRemux {
		t.Fatalf("direct probe overwrote the encoder decision: %q", got.Decision)
	}

	// An explicit switch back to direct delivery does replace it.
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionDirectPlay})
	if got := only(); got.Decision != core.DecisionDirectPlay || !got.DecisionVideoCopy || !got.DecisionAudioCopy {
		t.Fatalf("after direct switch: %+v, want directplay copying both streams", got)
	}

	// An empty decision never blanks the current one.
	h.m.SetDecision(ps.ID, core.Decision{})
	if got := only(); got.Decision != core.DecisionDirectPlay {
		t.Fatalf("empty decision blanked the current one: %q", got.Decision)
	}
}
