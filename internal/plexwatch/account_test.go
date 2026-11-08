package plexwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	hexA = "5d7768265af944001f1f6976"
	hexB = "5d77685333f255001e852e11"
	hexC = "5d776b59ad5437001f79c6f8"
)

func wantWrites(t *testing.T, h *harness, want ...string) {
	t.Helper()
	got := h.plex.takeWrites()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plex writes %q, want %q", got, want)
	}
}

// Both directions against one fake Discover account: a change made on
// either side reaches the other once and never bounces back.
func TestAccountSyncBothDirectionsWithoutEcho(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 42)
	st := h.connect(t, 2, 42, 3)
	raw, _ := json.Marshal(st)
	if !st.Connected || st.Direction != 3 || strings.Contains(string(raw), viewerToken) {
		t.Fatalf("status %s", raw)
	}
	a, b := core.MovieKey(550), core.MovieKey(27205)
	h.guid.add(a, hexA)
	h.guid.add(b, hexB)

	// Ether → Plex.
	if err := h.st.Scrobble(ctx, 2, a); err != nil {
		t.Fatal(err)
	}
	h.deliverAll(t)
	wantWrites(t, h, "scrobble "+hexA+" ")
	if h.plex.tokens[ownerToken] || len(h.plex.tokens) != 1 || !h.plex.tokens[viewerToken] {
		t.Fatalf("tokens sent %v", h.plex.tokens)
	}
	before := h.local(t, 2, a)

	// Reading Ether's own write back changes nothing.
	h.clock.Add(time.Minute)
	h.importNow(t, 2, 42)
	if after := h.local(t, 2, a); after.ViewCount != 1 || !after.LastViewedAt.Equal(before.LastViewedAt) {
		t.Fatalf("own write was imported: %+v -> %+v", before, after)
	}
	h.deliverAll(t)
	wantWrites(t, h)

	// Plex → Ether: a view and an unwatch made in Plex.
	h.clock.Add(time.Minute)
	h.plex.watch(hexB, "movie", h.clock.Now())
	h.plex.unwatch(hexA)
	h.clock.Add(time.Minute)
	h.importNow(t, 2, 42)
	if ws := h.local(t, 2, b); ws.ViewCount != 1 {
		t.Fatalf("plex view not imported: %+v", ws)
	}
	if ws := h.local(t, 2, a); ws.ViewCount != 0 {
		t.Fatalf("plex unwatch not imported: %+v", ws)
	}
	h.deliverAll(t)
	wantWrites(t, h)

	// Ratings both ways, including a clear.
	h.clock.Add(time.Minute)
	if err := h.st.Rate(ctx, 2, b, 7); err != nil {
		t.Fatal(err)
	}
	h.deliverAll(t)
	wantWrites(t, h, "rate "+hexB+" 7")
	h.clock.Add(time.Minute)
	h.importNow(t, 2, 42)
	h.clock.Add(time.Minute)
	h.plex.rate(hexB, "movie", 4)
	h.clock.Add(time.Minute)
	h.importNow(t, 2, 42)
	if ws := h.local(t, 2, b); ws.UserRating != 4 {
		t.Fatalf("plex rating not imported: %+v", ws)
	}
	h.deliverAll(t)
	wantWrites(t, h)
	h.clock.Add(time.Minute)
	if err := h.st.Rate(ctx, 2, b, 0); err != nil {
		t.Fatal(err)
	}
	h.deliverAll(t)
	wantWrites(t, h, "rate "+hexB+" -1")

	// Quiet afterwards in both directions.
	for i := 0; i < 2; i++ {
		h.clock.Add(time.Minute)
		h.importNow(t, 2, 42)
		h.deliverAll(t)
	}
	wantWrites(t, h)
	if ws := h.local(t, 2, b); ws.UserRating != 0 || ws.ViewCount != 1 {
		t.Fatalf("final %+v", ws)
	}
	// A viewer without a connection is never journaled or sent.
	if err := h.st.Scrobble(ctx, 8, a); err != nil {
		t.Fatal(err)
	}
	h.deliverAll(t)
	wantWrites(t, h)
}

// An unconfirmed send is read back before it is sent again: once if Plex
// lacks it, never if Plex already holds it or changed the title since.
func TestUncertainSendIsReadBack(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 42)
	h.connect(t, 2, 42, 1)
	a, b, c := core.MovieKey(1), core.MovieKey(2), core.MovieKey(3)
	h.guid.add(a, hexA)
	h.guid.add(b, hexB)
	h.guid.add(c, hexC)
	step := func(key core.RatingKey, applied bool) {
		t.Helper()
		h.plex.mu.Lock()
		h.plex.failNext, h.plex.applyThenFail = 503, applied
		h.plex.mu.Unlock()
		if err := h.st.Scrobble(ctx, 2, key); err != nil {
			t.Fatal(err)
		}
		h.deliverAll(t)
		if len(h.plex.takeWrites()) != 1 {
			t.Fatal("first attempt not sent")
		}
		if st, _ := h.svc.Status(ctx, 2, 42); st.Problem != "network" || st.Pending != 1 {
			t.Fatalf("after 503: %+v", st)
		}
	}

	step(a, true) // landed despite the 503
	h.clock.Add(3 * time.Minute)
	h.deliverAll(t)
	wantWrites(t, h)

	step(b, false) // did not land
	h.clock.Add(3 * time.Minute)
	h.deliverAll(t)
	wantWrites(t, h, "scrobble "+hexB+" ")

	step(c, true) // landed, then undone in Plex: Plex is newer
	h.plex.unwatch(hexC)
	h.clock.Add(3 * time.Minute)
	h.deliverAll(t)
	wantWrites(t, h)

	if reads := h.plex.takeReads(); len(reads) != 3 {
		t.Fatalf("read-backs %v", reads)
	}
	st, err := h.svc.Status(ctx, 2, 42)
	if err != nil || st.Pending != 0 || st.Failed != 0 || st.Problem != "" || st.LastSend == 0 {
		t.Fatalf("final %+v %v", st, err)
	}
}

// Turning sending off while an intent is being prepared stops it; the
// intent is not revived by turning sending on again.
func TestDisableFencesQueuedSend(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 42)
	h.connect(t, 2, 42, 1)
	a := core.MovieKey(1)
	h.guid.add(a, hexA)
	if err := h.st.Scrobble(ctx, 2, a); err != nil {
		t.Fatal(err)
	}
	h.guid.block, h.guid.entered = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := h.svc.DeliverOnce(ctx); done <- err }()
	<-h.guid.entered
	if _, err := h.svc.SetDirection(ctx, 2, 42, 0, 10); err != nil {
		t.Fatal(err)
	}
	h.guid.mu.Lock()
	block := h.guid.block
	h.guid.block, h.guid.entered = nil, nil
	h.guid.mu.Unlock()
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetDirection(ctx, 2, 42, 1, 10); err != nil {
		t.Fatal(err)
	}
	h.deliverAll(t)
	wantWrites(t, h)
	if err := h.st.Unscrobble(ctx, 2, a); err != nil {
		t.Fatal(err)
	}
	h.deliverAll(t)
	wantWrites(t, h, "unscrobble "+hexA+" ")
}

// A refused token marks the connection for reconnecting, stops all
// traffic, and reconnecting the same account keeps the chosen direction.
func TestRefusedTokenNeedsReconnect(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 42)
	h.connect(t, 2, 42, 3)
	a := core.MovieKey(1)
	h.guid.add(a, hexA)
	h.plex.mu.Lock()
	h.plex.rejectAll = 401
	h.plex.mu.Unlock()
	if err := h.st.Scrobble(ctx, 2, a); err != nil {
		t.Fatal(err)
	}
	h.deliverAll(t)
	st, err := h.svc.Status(ctx, 2, 42)
	if err != nil || !st.NeedsReconnect || st.Problem != "token" || st.Connected || st.Direction != 3 || st.Pending != 0 {
		t.Fatalf("after 401: %+v %v", st, err)
	}
	for i := 0; i < 3; i++ {
		h.clock.Add(time.Hour)
		h.deliverAll(t)
		h.svc.importDue(ctx)
		if err := h.st.Scrobble(ctx, 2, core.MovieKey(int64(10+i))); err != nil {
			t.Fatal(err)
		}
	}
	if h.plex.rejected != 1 || strings.Count(h.logs.String(), "\n") > 1 {
		t.Fatalf("kept calling Plex (%d) or logging:\n%s", h.plex.rejected, h.logs.String())
	}

	h.plex.mu.Lock()
	h.plex.rejectAll = 0
	h.plex.mu.Unlock()
	if st = h.connect(t, 2, 42, 0); !st.Connected || st.Direction != 3 || st.NeedsReconnect {
		t.Fatalf("reconnect %+v", st)
	}
	h.plex.mu.Lock()
	h.plex.rejectAll = 403
	h.plex.mu.Unlock()
	if _, err := h.svc.ImportCurrent(ctx, 2, 42); !errors.Is(err, ErrRevoked) {
		t.Fatalf("import 403: %v", err)
	}
	if st, _ = h.svc.Status(ctx, 2, 42); !st.NeedsReconnect {
		t.Fatalf("after 403: %+v", st)
	}
}

// 429 pauses every send for the viewer until Retry-After has passed and
// proves nothing landed, so the retry is sent without a read-back.
func TestRateLimitPausesViewer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 42)
	h.connect(t, 2, 42, 1)
	a, b := core.MovieKey(1), core.MovieKey(2)
	h.guid.add(a, hexA)
	h.guid.add(b, hexB)
	h.plex.mu.Lock()
	h.plex.failNext, h.plex.retryAfter = 429, "120"
	h.plex.mu.Unlock()
	if err := h.st.ScrobbleMany(ctx, 2, []core.RatingKey{a, b}); err != nil {
		t.Fatal(err)
	}
	h.deliverAll(t)
	wantWrites(t, h, "scrobble "+hexA+" ")
	h.clock.Add(time.Minute)
	h.deliverAll(t)
	wantWrites(t, h)
	if st, _ := h.svc.Status(ctx, 2, 42); st.Problem != "rate_limited" || st.Pending != 2 {
		t.Fatalf("paused %+v", st)
	}
	h.clock.Add(2 * time.Minute)
	h.deliverAll(t)
	wantWrites(t, h, "scrobble "+hexA+" ", "scrobble "+hexB+" ")
	if reads := h.plex.takeReads(); len(reads) != 0 {
		t.Fatalf("read-back after 429: %v", reads)
	}
}

// The import sweep reads movies and shows in capped batches from a saved
// cursor, and reads episodes only for shows whose watched count moved.
func TestImportSweepBatchesAndExpandsChangedShows(t *testing.T) {
	h := newHarness(t, 42)
	h.connect(t, 2, 42, 2)
	for i := int64(1); i <= 120; i++ {
		h.guid.add(core.MovieKey(i), fmt.Sprintf("%024x", i))
	}
	show, quiet := core.ShowKey(1396), core.ShowKey(2000)
	h.guid.add(show, fmt.Sprintf("%024x", 0xa000))
	h.guid.add(quiet, fmt.Sprintf("%024x", 0xb000))
	for e := 1; e <= 3; e++ {
		h.guid.add(core.EpisodeKey(1396, 1, e), fmt.Sprintf("%024x", 0xa000+e))
		h.guid.add(core.EpisodeKey(2000, 1, e), fmt.Sprintf("%024x", 0xb000+e))
	}
	h.plex.watch(fmt.Sprintf("%024x", 7), "movie", h.clock.Now().Add(-time.Hour))
	h.plex.watch(fmt.Sprintf("%024x", 0xa002), "episode", h.clock.Now().Add(-time.Hour))
	h.plex.mu.Lock()
	h.plex.title(fmt.Sprintf("%024x", 0xa000), "show").leaves = 1
	h.plex.title(fmt.Sprintf("%024x", 0xa000), "show").lastViewedAt = h.clock.Now().Add(-time.Hour).Unix()
	h.plex.mu.Unlock()

	// A small budget leaves the sweep part-way; the next pass resumes.
	h.svc.importBudget = 2
	st := h.importNow(t, 2, 42)
	if st.LastFullImport != 0 || st.NextImport > h.clock.Now().Add(2*time.Minute).Unix() {
		t.Fatalf("partial pass %+v", st)
	}
	h.svc.importBudget = importRequests
	st = h.importNow(t, 2, 42)
	if st.LastFullImport == 0 {
		t.Fatalf("sweep did not finish %+v", st)
	}
	reads := h.plex.takeReads()
	total := 0
	for _, r := range reads {
		if len(r) > maxStateBatch {
			t.Fatalf("batch of %d", len(r))
		}
		total += len(r)
	}
	// 120 movies + 2 shows, then the changed show's 3 episodes; after the
	// sweep, the active-show pass reads them again (the viewer now has
	// activity on that show). The quiet show's episodes are never read.
	if len(reads) != 5 || total != 128 || reads[3][0] != fmt.Sprintf("%024x", 0xa001) || reads[4][0] != fmt.Sprintf("%024x", 0xa001) {
		t.Fatalf("reads %d keys %d last %v", len(reads), total, reads[len(reads)-1])
	}
	if h.local(t, 2, core.MovieKey(7)).ViewCount != 1 || h.local(t, 2, core.EpisodeKey(1396, 1, 2)).ViewCount != 1 {
		t.Fatal("plex views not imported")
	}

	// Unchanged shows are not expanded by the sweep again; the active show
	// is, once per sweep.
	h.clock.Add(15 * time.Minute)
	h.importNow(t, 2, 42)
	if reads = h.plex.takeReads(); len(reads) != 4 || reads[3][0] != fmt.Sprintf("%024x", 0xa001) {
		t.Fatalf("second sweep reads %v", reads)
	}
	// A newly watched episode moves the show's count and is picked up.
	h.plex.watch(fmt.Sprintf("%024x", 0xa003), "episode", h.clock.Now())
	h.plex.mu.Lock()
	h.plex.title(fmt.Sprintf("%024x", 0xa000), "show").leaves = 2
	h.plex.mu.Unlock()
	h.clock.Add(15 * time.Minute)
	h.importNow(t, 2, 42)
	if h.local(t, 2, core.EpisodeKey(1396, 1, 3)).ViewCount != 1 {
		t.Fatal("new episode view not imported")
	}
	wantWrites(t, h)
}

func TestPlexRatingScale(t *testing.T) {
	for in, want := range map[float64]int{0: 0, -1: 0, 0.4: 1, 1: 1, 4.5: 5, 7: 7, 7.6: 8, 10: 10, 12: 10} {
		if got := plexRating(in); got != want {
			t.Errorf("plexRating(%v) = %d, want %d", in, got, want)
		}
	}
}
