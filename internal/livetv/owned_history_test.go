package livetv

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// tickingRelay lists 2 s segments at real time, like a live encoder (two
// are ready at once, for the first playlist).
type tickingRelay struct {
	core.HLSSession
	mu     sync.Mutex
	start  time.Time
	closed bool
}

func (r *tickingRelay) ServePlaylist(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	if r.start.IsZero() {
		r.start = time.Now()
	}
	n := 2 + int(time.Since(r.start)/(2*time.Second))
	r.mu.Unlock()
	fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n")
	for i := max(0, n-4); i < n; i++ {
		fmt.Fprintf(w, "#EXTINF:2,\n%05d.ts\n", i)
	}
}
func (r *tickingRelay) ServeSegment(w http.ResponseWriter, _ *http.Request, n int) {
	fmt.Fprintf(w, "relay-%d", n)
}
func (r *tickingRelay) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}

type fakeSteadyFeed struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakeSteadyFeed) Playlist(context.Context) ([]byte, error) {
	return []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:40\n#EXTINF:2,\n40.ts\n#EXTINF:2,\n41.ts\n"), nil
}
func (f *fakeSteadyFeed) Segment(_ context.Context, seq int64) ([]byte, error) {
	return []byte(fmt.Sprintf("steady-%d", seq)), nil
}
func (f *fakeSteadyFeed) Ready(context.Context) error { return nil }
func (f *fakeSteadyFeed) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// A Direct relay handed off to Steady keeps the client's stream: the feed is
// spliced into the same owned timeline behind a discontinuity, the relay
// encoder is retired, and the relay's own recovery stays out of the way.
func TestRelayHandOffToSteadySplicesOneStream(t *testing.T) {
	s := testService(t, Options{})
	relay := &tickingRelay{}
	ss := &session{info: SessionInfo{ID: "handoff", Channel: Channel{ID: "c"}}}
	ownedRelayFixture(t, s, ss, relay, core.Decision{Container: "mpegts", VideoCopy: true, AudioCopy: true}, relaySegment)
	p := ss.owned.relay
	if err := ss.owned.tl.waitStarted(t.Context()); err != nil {
		t.Fatal(err)
	}
	feed := &fakeSteadyFeed{}
	p.mu.Lock()
	p.steadyReady, p.steadyClass = feed, "damage"
	p.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	var steadySeq int64 = -1
	for time.Now().Before(deadline) && steadySeq < 0 {
		time.Sleep(50 * time.Millisecond)
		ss.owned.tl.mu.Lock()
		for _, seg := range ss.owned.tl.window {
			if strings.HasPrefix(string(seg.data), "steady-") {
				if !seg.disc && steadySeq < 0 {
					t.Error("the hand-off was not marked with a discontinuity")
				}
				steadySeq = seg.seq
				break
			}
		}
		ss.owned.tl.mu.Unlock()
	}
	if steadySeq < 0 {
		t.Fatal("the Steady feed never reached the owned timeline")
	}
	relay.mu.Lock()
	closed := relay.closed
	relay.mu.Unlock()
	p.mu.Lock()
	onSteady, cur := p.steady == SteadyFeed(feed), p.hls
	p.mu.Unlock()
	if !closed || !onSteady || cur != nil {
		t.Fatalf("relay retired=%v producer on steady=%v", closed, onSteady)
	}
	ss.owned.close()
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if !feed.closed {
		t.Fatal("closing the session left the Steady lease held")
	}
}

// A source whose history shows damage or keyframe gaps starts its Direct
// relay re-encoded, and the owner sees why; a clean source stays copy.
func TestHistoryStartsDamagedSourceReencoded(t *testing.T) {
	tc := &decisionTranscoder{}
	s := testService(t, Options{Transcoder: tc})
	if _, err := s.SaveChannel(t.Context(), testChannel()); err != nil {
		t.Fatal(err)
	}
	s.ReconcileEnabled()
	src := StreamSource{ID: "src", URL: "http://127.0.0.1:1/live.ts", Enabled: true}
	ss := &session{info: SessionInfo{ID: "h", Channel: Channel{ID: "c", Kind: "iptv"}}, inputSource: src}
	info := core.MediaInfo{Container: "mpegts", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 720}, {Index: 1, Type: "audio", Codec: "aac", Channels: 2, Default: true}}}
	copyDec := core.Decision{Kind: core.DecisionRemux, Container: "mpegts", VideoCopy: true, AudioCopy: true}
	if dec, ev := s.historyDecision(ss, info, copyDec); ev != "" || !dec.VideoCopy || s.learnedHealth("c", src) != nil {
		t.Fatalf("clean source left copy: %q %+v", ev, dec)
	}
	s.noteSourceEvidence("c", src, SourceEvidence{Media: time.Minute, DamageEvents: 1})
	dec, ev := s.historyDecision(ss, info, copyDec)
	if ev != "damage" || dec.VideoCopy {
		t.Fatalf("damaged source: evidence %q, video copy %v", ev, dec.VideoCopy)
	}
	if l := s.learnedHealth("c", src); l == nil || !l.Damage || !l.StartsConverted {
		t.Fatalf("owner status lacks the learned damage: %+v", l)
	}
	gop := StreamSource{ID: "gop", URL: "http://127.0.0.1:1/gop.ts", Enabled: true}
	ss.inputSource = gop
	s.noteSourceEvidence("c", gop, SourceEvidence{Media: time.Minute, KeyframeGap: 10 * time.Second})
	if _, ev := s.historyDecision(ss, info, copyDec); ev != "keyframe" {
		t.Fatalf("long-GOP source: evidence %q", ev)
	}
	if l := s.learnedHealth("c", gop); l == nil || !l.LongGOP || l.Damage {
		t.Fatalf("owner status for the long-GOP source: %+v", l)
	}
}

// decisionTranscoder converts when direct stream is off, like the real one.
type decisionTranscoder struct{ fakeTranscoder }

func (decisionTranscoder) Decide(_ core.MediaInfo, _ core.ClientCaps, o core.DecideOptions) core.Decision {
	if o.DirectStream {
		return core.Decision{Kind: core.DecisionRemux, Container: "mpegts", VideoCopy: true, AudioCopy: true}
	}
	return core.Decision{Kind: core.DecisionTranscode, Container: "mpegts", VideoCodec: "h264", AudioCopy: true}
}

// A shared passthrough timeline escalates as a whole: once the feed is in,
// every viewer of the channel reads Steady's segments on the same owned
// timeline, behind one discontinuity, and the lease ends with the channel.
func TestPassthroughHandOffToSteadyMovesTheChannel(t *testing.T) {
	start := time.Now()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".ts") {
			fmt.Fprint(w, "provider")
			return
		}
		n := 3 + int(time.Since(start)/(2*time.Second))
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:%d\n", n-3)
		for i := n - 3; i < n; i++ {
			fmt.Fprintf(w, "#EXTINF:2,\ns%d.ts\n", i)
		}
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client(), Stability: func() string { return stabilityFastest }})
	c := Channel{ID: "shared", Kind: "iptv"}
	src := StreamSource{ID: "p", URL: up.URL + "/live.m3u8", Enabled: true}
	viewer := func(id string) *session {
		ss := &session{info: SessionInfo{ID: id, Channel: c}, inputSource: src, inputURL: src.URL, ctx: t.Context()}
		ss.owned = s.acquirePassthrough(ss)
		t.Cleanup(ss.owned.close)
		return ss
	}
	a, b := viewer("a"), viewer("b")
	if a.owned.tl != b.owned.tl {
		t.Fatal("viewers of one channel do not share its timeline")
	}
	if err := a.owned.tl.waitStarted(t.Context()); err != nil {
		t.Fatal(err)
	}
	st := s.owned()
	st.mu.Lock()
	p := st.channels[func() string {
		for k := range st.channels {
			return k
		}
		return ""
	}()].p
	st.mu.Unlock()
	feed := &fakeSteadyFeed{}
	p.steadyMu.Lock()
	p.steadyReady, p.steadyClass = feed, "stall"
	p.steadyMu.Unlock()
	tl := a.owned.tl
	deadline := time.Now().Add(6 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		time.Sleep(50 * time.Millisecond)
		tl.mu.Lock()
		for _, seg := range tl.window {
			if strings.HasPrefix(string(seg.data), "steady-") {
				found = true
				if !seg.disc {
					t.Error("the hand-off was not marked with a discontinuity")
				}
				break
			}
		}
		tl.mu.Unlock()
	}
	if !found || !p.onSteady() {
		t.Fatal("the channel's timeline never moved onto the Steady feed")
	}
	a.owned.close()
	b.owned.close()
	deadline = time.Now().Add(ownedLinger + 5*time.Second)
	for time.Now().Before(deadline) {
		feed.mu.Lock()
		closed := feed.closed
		feed.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the Steady lease outlived the channel's timeline")
}
