package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/subtitles"
)

type progressiveTestTranscoder struct {
	fakeBrokerTranscoder
	session core.HLSSession
}

func (p progressiveTestTranscoder) Session(string) (core.HLSSession, bool) { return p.session, true }

type progressiveTestSession struct{ fakeMediaSession }

func (*progressiveTestSession) TextSubtitleCues(index int, from, to time.Duration) ([]subtitles.Cue, error) {
	return []subtitles.Cue{{Start: 15 * time.Second, End: 17 * time.Second, Text: "hello\nworld"}}, nil
}

// Incremental complete JSON events, cancellation and capability-gated routing
// are the contract. The backend exposes only stored cues, no source reader.
func TestProgressiveSubtitleDelivery(t *testing.T) {
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}
	b := &Broker{deps: BrokerDeps{Playback: p, Transcoder: progressiveTestTranscoder{session: &progressiveTestSession{}}}}
	s := &brokerSession{playID: "p", caps: core.ClientCaps{Profiles: &core.ClientCapabilityProfiles{Subtitles: []core.ClientSubtitleProfile{{Format: progressiveSubtitleFormat, Method: "external"}}}}}
	s.public.Protocol = "jellyfin"
	stream := core.StreamInfo{Index: 2, Type: "subtitle", Codec: "subrip"}
	info := core.MediaInfo{Size: 4 << 30, Streams: []core.StreamInfo{stream}}
	if !b.progressiveSubtitle(s, info, stream) {
		t.Fatal("expensive source not progressive")
	}
	info.Size = 1024
	if b.progressiveSubtitle(s, info, stream) {
		t.Fatal("small source progressive")
	}
	info.Size = 4 << 30
	s.public.Protocol = "plex"
	if b.progressiveSubtitle(s, info, stream) {
		t.Fatal("changed Plex delivery")
	}
	s.public.Protocol = "jellyfin"
	profiles := s.caps.Profiles
	s.caps.Profiles = nil
	if b.progressiveSubtitle(s, info, stream) {
		t.Fatal("ordinary client progressive")
	}
	s.caps.Profiles = profiles
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		b.serveProgressiveSubtitle(w, r, s, stream)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"?from_ms=13000", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var data struct {
		TrackEvents []struct {
			Text               string
			StartPositionTicks int64
		}
	}
	if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if len(data.TrackEvents) != 1 || data.TrackEvents[0].Text != `hello\Nworld` || data.TrackEvents[0].StartPositionTicks != 150000000 {
		t.Fatalf("%+v", data)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader survived cancellation")
	}
}
