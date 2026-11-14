package livetv

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func historyService(t *testing.T, db *sql.DB, opts ...func(*Options)) *Service {
	t.Helper()
	o := Options{DB: db, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, f := range opts {
		f(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	// History persistence belongs to an active Live TV lifetime. Save a paused
	// fixture channel so these tests exercise loading and write-behind workers.
	c := testChannel()
	c.Enabled = false
	if _, err := s.SaveChannel(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	s.ReconcileEnabled()
	return s
}

// Evidence, cooldowns and modes survive a restart: Close flushes, New loads,
// and a source cooling down stays cooling down. The same test runs on
// PostgreSQL when ETHER_TEST_POSTGRES is set.
func TestSourceHistoryPersistsAcrossRestart(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Unix(2_000_000_000, 0)
	clock := func(o *Options) { o.Now = func() time.Time { return now } }
	src := StreamSource{ID: "primary", URL: "http://provider.example/live/1.ts", Enabled: true}

	first := historyService(t, db, clock)
	first.recordSourceHealth("ch", src, providerResponseError(458), false)
	first.noteSourceEvidence("ch", src, SourceEvidence{Media: 30 * time.Minute, DamageEvents: 90, KeyframeGap: 9 * time.Second, Stalls: 2, newSession: true})
	first.noteSourceMode("ch", src, "copy", "escalated")
	first.Close()

	second := historyService(t, db, clock)
	defer second.Close()
	h, ok := second.sourceHistory("ch", src)
	if !ok {
		t.Fatal("history lost across the restart")
	}
	if h.Failures != 1 || h.LastError != "refused" || h.Sessions != 1 || h.DamageEvents != 90 || h.Stalls != 2 ||
		h.MaxKeyframeGap() != 9*time.Second || h.LastMode != "copy" || h.LastOutcome != "escalated" {
		t.Fatalf("restored history %+v", h)
	}
	if got := h.DamagePerHour(); got < 179 || got > 181 {
		t.Fatalf("damage per hour %v, want 180", got)
	}
	if st := second.sourceStatus("ch", src); st.State != "cooling_down" || st.Failures != 1 {
		t.Fatalf("cooldown not restored: %+v", st)
	}
	// A replaced URL starts fresh.
	moved := src
	moved.URL = "http://provider.example/live/2.ts"
	if _, ok := second.sourceHistory("ch", moved); ok {
		t.Fatal("a replaced URL inherited the old URL's history")
	}
}

// A cluster node merges what another node learned, reading only rows
// changed since the last it saw, and a newer local entry is never replaced
// by an older row.
func TestSourceHistoryClusterRefresh(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	src := StreamSource{ID: "a", URL: "http://provider.example/a.ts", Enabled: true}
	other := StreamSource{ID: "b", URL: "http://provider.example/b.ts", Enabled: true}
	nodeA := historyService(t, db)
	defer nodeA.Close()
	nodeB := historyService(t, db)
	defer nodeB.Close()

	nodeB.noteSourceMode("ch", other, "steady", "ok")
	nodeA.noteSourceEvidence("ch", src, SourceEvidence{Media: time.Minute, Stalls: 3})
	nodeA.history.flush(context.Background(), db)
	nodeB.history.refresh(db)
	if h, ok := nodeB.sourceHistory("ch", src); !ok || h.Stalls != 3 {
		t.Fatalf("node B did not merge node A's evidence: %+v %v", h, ok)
	}
	// An older row for b must not replace B's own entry.
	nodeA.noteSourceMode("ch", other, "copy", "failed")
	stale, _ := nodeA.sourceHistory("ch", other)
	stale.Updated = time.Now().Add(-time.Hour)
	nodeA.history.entries.Store(stale.Key, &stale)
	nodeA.history.flush(context.Background(), db)
	nodeB.history.refresh(db)
	if h, _ := nodeB.sourceHistory("ch", other); h.LastMode != "steady" {
		t.Fatalf("an older row replaced a newer entry: %+v", h)
	}
}

// Counters halve once they cover more than the window of media, so a
// source that recovers stops looking damaged; a long keyframe gap ages out
// one half-window later.
func TestSourceHistoryDecays(t *testing.T) {
	var h SourceHistory
	h.add(SourceEvidence{Media: time.Hour, DamageEvents: 600, KeyframeGap: 10 * time.Second})
	for range 10 {
		h.add(SourceEvidence{Media: time.Hour})
	}
	if h.DamagePerHour() >= 100 || h.MediaSeconds >= historyWindow.Seconds() {
		t.Fatalf("damage did not decay: %v per hour over %v s", h.DamagePerHour(), h.MediaSeconds)
	}
	if h.MaxKeyframeGap() != 0 {
		t.Fatalf("keyframe gap did not age out: %v", h.MaxKeyframeGap())
	}
}

// The tap feeds history from what it sees: received media, continuity
// breaks, keyframe gaps and the session, under the session's original
// source entry rather than the resolved URL.
func TestTapRecordsSourceEvidence(t *testing.T) {
	g := &tsGen{cc: map[int]byte{}, dts: 900000, gop: 12}
	var stream []byte
	for i := range 120 {
		frame := g.next()
		if i%20 == 10 {
			g.cc[0x101] = (g.cc[0x101] + 3) & 0x0f // the provider loses audio packets
		}
		stream = append(stream, frame...)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(stream)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client()})
	original := StreamSource{ID: "a", URL: "plugin://addon/channel/1", Enabled: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ss := &session{ctx: ctx, cancel: cancel, inputSource: original, info: SessionInfo{ID: "ev", Channel: Channel{ID: "ev", Kind: "iptv"}}}
	resolved := original
	resolved.URL = up.URL + "/live.ts"
	if err := s.grantInput(ss, resolved, http.Header{}); err != nil {
		t.Fatal(err)
	}
	input, _ := s.relayInput(ss)
	res, err := http.Get(input)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, int64(len(stream))-4*tsPacket*8))
	res.Body.Close()
	s.revokeInput(ss)
	h, ok := s.sourceHistory("ev", original)
	if !ok {
		t.Fatal("no history under the session's source entry")
	}
	if h.Sessions != 1 || h.MediaSeconds < 3 || h.DamageEvents < 4 || h.MaxKeyframeGap() < 400*time.Millisecond {
		t.Fatalf("tap evidence %+v (gap %v)", h, h.MaxKeyframeGap())
	}
	if _, ok := s.sourceHistory("ev", resolved); ok {
		t.Fatal("evidence was keyed by the resolved URL")
	}
}
