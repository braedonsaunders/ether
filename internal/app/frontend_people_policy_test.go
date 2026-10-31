package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

func TestPeopleSnapshotRejectsRemoteWriterBeforeFinish(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(779)
	title := core.Title{Key: key, Type: core.TypeMovie, Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Thriller"}, Cast: []core.Person{{TMDB: 30, Name: "Person"}}}
	if err := a.catalog.UpsertTitle(ctx, title, false); err != nil {
		t.Fatal(err)
	}
	access, err := a.beginPeopleRead(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	rows, err := access.Titles(ctx, []core.RatingKey{key})
	if err != nil || len(rows) != 1 || !access.Policy.Allowed(viewer, &title) {
		t.Fatalf("initial snapshot %+v %v", rows, err)
	}
	// A second Service commits without refreshing this node's policy cache.
	writer, err := safety.New(ctx, a.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Put(ctx, viewer.ID, safety.Policy{BlockedGenres: []string{"Thriller"}}); err != nil {
		t.Fatal(err)
	}
	if !a.safety.Allowed(viewer, &title) {
		t.Fatal("fixture no longer has a stale reader")
	}
	if err := access.Finish(ctx); !errors.Is(err, viewerpolicy.ErrChanged) {
		t.Fatalf("writer won before fence: %v", err)
	}
}

type peopleConfigBackend struct {
	config.SharedBackend
	afterVersion func()
}

func (b *peopleConfigBackend) SharedVersion(ctx context.Context) (int64, error) {
	version, err := b.SharedBackend.SharedVersion(ctx)
	if err == nil && b.afterVersion != nil {
		b.afterVersion()
	}
	return version, err
}

func TestPeopleSnapshotRejectsNewEpochWithStaleConfig(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend := &peopleConfigBackend{SharedBackend: clusterconfig.NewShared(a.db)}
	if err := a.cfg.AttachShared(ctx, backend, config.SharedOptions{PollInterval: -1}); err != nil {
		t.Fatal(err)
	}
	backend.afterVersion = func() {
		backend.afterVersion = nil
		// Commit after the lease's authoritative version read but before its
		// caller can read the epoch. The fresh epoch alone cannot validate the
		// older local config; the in-transaction version comparison is required.
		if _, err := a.db.ExecContext(ctx, `UPDATE clusterconfig_shared SET version=version+1 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
	}
	access, err := a.beginPeopleRead(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	if err := access.Finish(ctx); !errors.Is(err, viewerpolicy.ErrChanged) {
		t.Fatalf("new epoch paired with stale config: %v", err)
	}
}

func TestPeopleSnapshotClockBounds(t *testing.T) {
	now := time.Now()
	if !validPeopleReadTimes(now, now.Add(time.Second)) {
		t.Fatal("bounded forward sample denied")
	}
	if validPeopleReadTimes(now, now.Add(-time.Second)) {
		t.Fatal("backwards clock accepted")
	}
	if validPeopleReadTimes(now, now.Add(5*time.Second)) {
		t.Fatal("unbounded read accepted")
	}
}

func TestPeoplePolicyFenceSerializesRemoteWriter(t *testing.T) {
	a := setupTestApp(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	epoch, err := viewerpolicy.Read(ctx, a.db)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := viewerpolicy.Fence(ctx, a.db, tx, epoch); err != nil {
		t.Fatal(err)
	}
	entered, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(entered)
		_, err := a.db.ExecContext(ctx, `INSERT INTO safety_policies(user_id,policy) VALUES(991,'{}') ON CONFLICT(user_id) DO UPDATE SET policy=excluded.policy`)
		done <- err
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("writer committed through held epoch fence: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	changed, err := viewerpolicy.Read(ctx, a.db)
	if err != nil || changed <= epoch {
		t.Fatalf("writer did not publish new epoch %d -> %d: %v", epoch, changed, err)
	}
}

func TestPeopleSnapshotPinsAlreadyRestrictedDurableViewer(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	stale, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(780)
	title := core.Title{Key: key, Type: core.TypeMovie, Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), ContentRating: "R", Cast: []core.Person{{TMDB: 30, Name: "Restricted Person"}}}
	if err := a.catalog.UpsertTitle(ctx, title, false); err != nil {
		t.Fatal(err)
	}
	availability := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = availability })
	exp, err := a.newSurfaceExperience("silo")
	if err != nil {
		t.Fatal(err)
	}
	// The write precedes the entire snapshot and leaves the process auth cache
	// stale. Merely reading the already-advanced epoch would not refresh it.
	if _, err := a.db.ExecContext(ctx, `UPDATE auth_users SET is_owner=0,allowed_sections='[2]',max_content_rating='PG' WHERE id=?`, stale.ID); err != nil {
		t.Fatal(err)
	}
	cached, err := a.users.ActiveUser(ctx, stale.ID)
	if err != nil || !cached.IsOwner {
		t.Fatalf("fixture lost stale auth cache %+v %v", cached, err)
	}
	rows, err := exp.(frontend.PeopleReader).People(ctx, stale, core.Device{ID: "tv"}, frontend.PeopleQuery{})
	if err != nil || len(rows) != 0 {
		t.Fatalf("already-restricted viewer saw people %+v %v", rows, err)
	}
	access, err := a.beginPeopleRead(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	if access.Viewer.IsOwner || access.Viewer.MaxContentRating != "PG" || len(access.Viewer.AllowedSections) != 1 || access.Viewer.AllowedSections[0] != 2 {
		t.Fatalf("stale viewer retained %+v", access.Viewer)
	}
	if err := access.Finish(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPeopleSnapshotUsesLeasedSectionsAfterSpikeTransition(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := a.beginPeopleRead(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := before.SectionFor(core.TypeEpisode); !ok || id != 2 {
		t.Fatal("initial TV section missing")
	}
	before.Close()
	if err := a.cfg.Update(func(cfg *config.Config) error { cfg.Spike.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := a.beginPeopleRead(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	for _, kind := range []core.MediaType{core.TypeShow, core.TypeSeason, core.TypeEpisode} {
		if _, ok := after.SectionFor(kind); ok {
			t.Fatalf("stale TV classifier for %s", kind)
		}
	}
	if id, ok := after.SectionFor(core.MediaType("virtual-other")); !ok || id != 1 {
		t.Fatal("virtual kind classification changed")
	}
}
