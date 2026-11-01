package playback

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// saveResume stores a resume record for user/key on release id, last
// touched at updated.
func (h *harness) saveResume(user int64, key core.RatingKey, id string, size int64, updated time.Time) {
	h.t.Helper()
	rec := resumeRecord{userID: user, key: key, releaseID: id, release: core.Release{ID: id}, size: size, updatedAt: updated}
	if err := h.m.resume.save(context.Background(), rec, []byte("nzb-"+id)); err != nil {
		h.t.Fatal(err)
	}
}

func TestReconcileSkipsAbandonedInProgressTitles(t *testing.T) {
	h := newHarness(t, nil, nil)
	ctx := context.Background()
	now := h.clock.Now()
	h.state.set(owner.ID, movieKey, time.Minute)
	h.state.set(owner.ID, movie2, time.Minute)
	// Watch state activity counts even when the resume record is old.
	h.state.setStatus(owner.ID, core.WatchStatus{Key: movie3, ViewOffset: time.Minute, UpdatedAt: now.Add(-2 * time.Hour)})
	// Seed watch state first: the manager's initial background reconcile can
	// otherwise legitimately expire old records before this fixture is ready.
	h.saveResume(owner.ID, movieKey, "recent", 100, now.Add(-24*time.Hour))
	h.saveResume(owner.ID, movie2, "abandoned", 100, now.Add(-40*24*time.Hour))
	h.saveResume(owner.ID, movie3, "rewatched", 100, now.Add(-40*24*time.Hour))
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.cache.pinned("recent", PinReasonInProgress) || !h.cache.pinned("rewatched", PinReasonInProgress) {
		t.Fatalf("recently watched titles not pinned: %v", h.cache.pins)
	}
	if h.cache.pinned("abandoned", PinReasonInProgress) {
		t.Fatal("title idle past InProgressPinMaxAge still pinned")
	}
	// The resume record itself is kept while the title is in progress.
	if rp, _ := h.m.ResumePoint(ctx, owner.ID, movie2); rp == nil {
		t.Fatal("abandoned in-progress resume record deleted")
	}

	// Raising the max age re-pins it.
	cfg := h.cfg
	cfg.Cache.InProgressPinMaxAge = config.Duration(50 * 24 * time.Hour)
	h.m.SetConfig(cfg)
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.cache.pinned("abandoned", PinReasonInProgress) {
		t.Fatal("max age from config not applied")
	}
}

func TestReconcileBoundsPinsToBudgetShare(t *testing.T) {
	h := newHarness(t, nil, nil)
	ctx := context.Background()
	now := h.clock.Now()
	// default 50% share -> 500 bytes of pins
	h.cache.setSizes(1000, map[string]int64{"newest": 300, "middle": 300, "oldest": 150})
	h.saveResume(owner.ID, movieKey, "newest", 0, now.Add(-time.Hour))
	h.saveResume(owner.ID, movie2, "middle", 0, now.Add(-2*time.Hour))
	h.saveResume(owner.ID, movie3, "oldest", 0, now.Add(-3*time.Hour))
	for _, k := range []core.RatingKey{movieKey, movie2, movie3} {
		h.state.set(owner.ID, k, time.Minute)
	}
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.cache.pinned("newest", PinReasonInProgress) || !h.cache.pinned("oldest", PinReasonInProgress) {
		t.Fatalf("pins = %v, want newest and oldest", h.cache.pins)
	}
	if h.cache.pinned("middle", PinReasonInProgress) {
		t.Fatal("pin over the budget share kept")
	}

	// A larger share admits everything.
	cfg := h.cfg
	cfg.Cache.InProgressPinPercent = 100
	h.m.SetConfig(cfg)
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.cache.pinned("middle", PinReasonInProgress) {
		t.Fatal("pin share from config not applied")
	}
}

func TestFirstReconcileDropsPinsWithoutResumeRecord(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.cache.Pin("gone", PinReasonInProgress)
	h.cache.Pin("gone", PinReasonLive)
	h.cache.Pin("gone", "owner")
	h.m.pinMu.Lock()
	h.m.pinsSwept = false
	h.m.pinMu.Unlock()
	if err := h.m.ReconcilePins(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.cache.pinned("gone", PinReasonInProgress) || h.cache.pinned("gone", PinReasonLive) {
		t.Fatalf("orphan playback pins kept: %v", h.cache.pins)
	}
	if !h.cache.pinned("gone", "owner") {
		t.Fatal("foreign pin reason removed")
	}
}

func TestResumeExpiryRunsWithoutCache(t *testing.T) {
	h := newHarness(t, nil, nil, func(h *harness, o *Options) {
		h.noCache = true
		o.ResumeRetention = 24 * time.Hour
	})
	ctx := context.Background()
	h.saveResume(owner.ID, movieKey, "A", 0, h.clock.Now().Add(-48*time.Hour))
	if err := h.m.ReconcilePins(ctx); err != nil {
		t.Fatal(err)
	}
	if rp, _ := h.m.ResumePoint(ctx, owner.ID, movieKey); rp != nil {
		t.Fatal("expired resume record kept when no segment cache is configured")
	}
	if _, err := os.Stat(h.m.resume.nzbPath(nzbHash([]byte("nzb-A")))); !os.IsNotExist(err) {
		t.Fatalf("expired NZB kept: %v", err)
	}
}

// The pin loop can expire a row after save commits and before save stores
// the in-memory copy. That late copy must not bring the resume back.
func TestResumeExpiryBeatsLateLivePublish(t *testing.T) {
	h := newHarness(t, nil, nil, func(h *harness, o *Options) {
		h.noCache = true
		o.ResumeRetention = 24 * time.Hour
	})
	h.m.resume.beforeLiveNote = func() {
		if err := h.m.ReconcilePins(context.Background()); err != nil {
			t.Error(err)
		}
	}
	h.saveResume(owner.ID, movieKey, "A", 0, h.clock.Now().Add(-48*time.Hour))
	if rp, _ := h.m.ResumePoint(context.Background(), owner.ID, movieKey); rp != nil {
		t.Fatal("expired resume revived by the save that just lost the row")
	}
}

func TestResumeStoreSweepsStaleTempsAndOrphans(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "playback", "nzb")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	write := func(name string, mod time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		return p
	}
	staleTmp := write("nzb-123.tmp", old)
	freshTmp := write("nzb-456.tmp", time.Now())
	orphan := write(nzbHash([]byte("orphan"))+".nzb", old)

	rs, err := newResumeStore(db, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.close(context.Background()) })
	if err := rs.save(context.Background(), resumeRecord{userID: 1, key: movieKey, releaseID: "K", release: core.Release{ID: "K"}, updatedAt: time.Now()}, []byte("kept")); err != nil {
		t.Fatal(err)
	}
	kept := rs.nzbPath(nzbHash([]byte("kept")))
	os.Chtimes(kept, old, old)
	rs.sweep(context.Background())

	for _, p := range []string{staleTmp, orphan} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s not swept: %v", filepath.Base(p), err)
		}
	}
	for _, p := range []string{freshTmp, kept} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", filepath.Base(p), err)
		}
	}
}
