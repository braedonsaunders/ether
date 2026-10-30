package transcode

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// A DASH ladder representation encodes with its own rung's VBV plan: DASH
// track 2 is rung 1 (track 1 is audio), not rung 2.
func TestDASHLadderRepUsesItsRungVBV(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true, VBVReservoir: true})
	s := startDASHSession(t, m, ladderEncodeDASHRequest(t, "dvbv", 10*time.Minute))
	s.mu.Lock()
	if len(s.rungs) < 2 {
		s.mu.Unlock()
		t.Fatalf("fixture needs a ladder, got %d rungs", len(s.rungs))
	}
	want := fmt.Sprintf("%dk", s.rungs[1].peakKbps)
	s.mu.Unlock()
	rec := httptest.NewRecorder()
	s.ServeMedia(rec, httptest.NewRequest(http.MethodGet, "/2/0.m4s", nil), 2, 0)
	if rec.Code != 200 {
		t.Fatalf("rep 2 segment 0: %d", rec.Code)
	}
	if got := argValue(fr.lastStart(), "-maxrate"); got != want {
		t.Fatalf("rep 2 -maxrate = %q, want rung 1's %q\n%v", got, want, fr.lastStart())
	}
}

// A video-only DASH title cannot number ladder reps after an audio track;
// every representation the MPD advertises must be servable.
func TestDASHVideoOnlyLadderRepsAreServable(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true})
	req := ladderEncodeDASHRequest(t, "dvonly", 10*time.Minute)
	var streams []core.StreamInfo
	for _, st := range req.Info.Streams {
		if st.Type == "video" {
			streams = append(streams, st)
		}
	}
	req.Info.Streams = streams
	req.Info.BitrateKbps = 40000
	req.Info.Streams[0].Height, req.Info.Streams[0].Width = 2160, 3840
	req.Decision.AudioStream = -1
	req.Decision.MaxHeight, req.Decision.BitrateKbps = 2160, 40000
	s := startDASHSession(t, m, req)
	s.mu.Lock()
	reps := append([]dashRep(nil), s.dash.reps...)
	s.mu.Unlock()
	for i, r := range reps {
		if r.ID != i {
			t.Fatalf("rep %d has ID %d: %+v", i, r.ID, reps)
		}
	}
	for _, r := range reps {
		if rec := media(t, s, r.ID, 0); rec.Code != 200 {
			t.Fatalf("advertised rep %d segment 0: %d", r.ID, rec.Code)
		}
	}
}
