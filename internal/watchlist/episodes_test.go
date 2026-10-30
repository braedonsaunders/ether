package watchlist

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type showCatalog struct {
	fakeCatalog
	episodes []core.Title
}

func (c *showCatalog) Grandchildren(context.Context, core.RatingKey) ([]core.Title, error) {
	return append([]core.Title(nil), c.episodes...), nil
}

type episodeResolver struct {
	core.Resolver
	mu      sync.Mutex
	missing map[core.RatingKey]bool
	asked   []core.RatingKey
}

func (r *episodeResolver) Resolve(_ context.Context, q core.ResolveRequest) (*core.Resolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, q.Title.Key)
	if r.missing[q.Title.Key] {
		return nil, core.ErrNotAvailable
	}
	return &core.Resolution{Title: q.Title.Key, Release: core.Release{ID: "rel-" + q.Title.Key.String()}, NZB: []byte("nzb")}, nil
}

func (r *episodeResolver) take() []core.RatingKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.asked
	r.asked = nil
	return out
}

type recordingNotifier struct {
	mu     sync.Mutex
	titles []string
}

func (n *recordingNotifier) TitleAvailable(_ context.Context, title string) error {
	n.mu.Lock()
	n.titles = append(n.titles, title)
	n.mu.Unlock()
	return nil
}

func TestWatchlistedShowAnnouncesEachNewEpisodeOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ether.db")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	const show = 1399
	ep := func(season, episode int, aired time.Duration, title string) core.Title {
		return core.Title{Key: core.EpisodeKey(show, season, episode), Type: core.TypeEpisode, Title: title, ShowTitle: "The Show", Season: season, Episode: episode, Released: now.Add(aired)}
	}
	showTitle := core.Title{Key: core.ShowKey(show), Type: core.TypeShow, Title: "The Show", ContentRating: "TV-14", Released: now.Add(-60 * 24 * time.Hour)}
	cat := &showCatalog{fakeCatalog: fakeCatalog{titles: map[core.RatingKey]core.Title{showTitle.Key: showTitle}}, episodes: []core.Title{
		ep(1, 1, -30*24*time.Hour, "Pilot"),
		ep(1, 2, -7*24*time.Hour, "Second"),
		ep(0, 1, -24*time.Hour, "Behind the scenes"), // special: never "new"
		ep(1, 3, 3*24*time.Hour, "Third"),            // not aired yet
	}}
	res := &episodeResolver{missing: map[core.RatingKey]bool{}}
	note := &recordingNotifier{}
	eng := &fakeEngine{}
	open := func() *Service {
		db, err := store.OpenPath(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return New(db, Options{Catalog: cat, Resolver: res, Engine: eng, Notifier: note, Now: clock})
	}
	s := open()
	if err := s.SaveSettings(ctx, Settings{Enabled: true, Notify: true, Prewarm: true, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	alice, bob := &core.User{ID: 1, MaxContentRating: "TV-MA"}, &core.User{ID: 2, MaxContentRating: "TV-MA"}
	for _, u := range []*core.User{alice, bob} {
		if err := s.Remind(ctx, u, showTitle.Key); err != nil {
			t.Fatal(err)
		}
	}
	check := func(svc *Service, users ...*core.User) {
		t.Helper()
		for _, u := range users {
			if err := svc.Check(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The show becomes available through its pilot; episodes so far are
	// not announced individually.
	check(s, alice, bob)
	if got := res.take(); len(got) != 2 || got[0] != core.EpisodeKey(show, 1, 1) {
		t.Fatalf("resolved %v, want the pilot for each viewer", got)
	}
	if len(note.titles) != 2 || note.titles[0] != "The Show" {
		t.Fatalf("alerts %q", note.titles)
	}

	// Later checks re-validate as before and announce nothing new.
	now = now.Add(7 * time.Hour)
	check(s, alice, bob)
	if len(note.titles) != 2 {
		t.Fatalf("alerts %q", note.titles)
	}
	res.take()

	// Episode 3 airs; no release is out yet.
	third := core.EpisodeKey(show, 1, 3)
	res.missing[third] = true
	now = now.Add(3 * 24 * time.Hour)
	check(s, alice)
	if got := res.take(); len(got) != 1 || got[0] != third {
		t.Fatalf("resolved %v, want only the new episode", got)
	}
	// Recently aired: checked again within the hour, not every 6 hours.
	now = now.Add(30 * time.Minute)
	check(s, alice)
	if got := res.take(); len(got) != 0 {
		t.Fatalf("re-checked too soon: %v", got)
	}
	now = now.Add(31 * time.Minute)
	delete(res.missing, third)
	check(s, alice)
	if got := res.take(); len(got) != 1 || got[0] != third {
		t.Fatalf("resolved %v", got)
	}
	if len(note.titles) != 3 || note.titles[2] != "The Show S01E03 · Third" {
		t.Fatalf("alerts %q", note.titles)
	}
	if eng.calls != 3 {
		t.Fatalf("prewarmed %d times, want pilot x2 + new episode", eng.calls)
	}

	// The other viewer finds it too, and after a restart nothing repeats.
	check(s, bob)
	s2 := open()
	now = now.Add(7 * time.Hour)
	check(s2, alice, bob)
	if len(note.titles) != 3 {
		t.Fatalf("duplicate alerts %q", note.titles)
	}
	for _, title := range note.titles {
		if strings.Contains(title, "S00") {
			t.Fatalf("special announced: %q", title)
		}
	}

	// A viewer who may not see the show is never asked about it.
	res.take()
	kid := &core.User{ID: 3, MaxContentRating: "TV-Y"}
	if err := s2.Remind(ctx, kid, showTitle.Key); err == nil {
		t.Fatal("restricted viewer could add the show")
	}
	check(s2, kid)
	if got := res.take(); len(got) != 0 {
		t.Fatalf("resolved %v for a restricted viewer", got)
	}
}

type flakyNotifier struct {
	recordingNotifier
	fail bool
}

func (n *flakyNotifier) TitleAvailable(ctx context.Context, title string) error {
	if n.fail {
		return errors.New("notification queue unavailable")
	}
	return n.recordingNotifier.TitleAvailable(ctx, title)
}

// A failed alert, or a database failure while creating it, is retried and
// never lost; the alert still goes out once.
func TestNewEpisodeAlertSurvivesFailures(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	const show = 4242
	ep := func(e int, aired time.Duration) core.Title {
		return core.Title{Key: core.EpisodeKey(show, 1, e), Type: core.TypeEpisode, ShowTitle: "Flaky", Season: 1, Episode: e, Released: now.Add(aired)}
	}
	showTitle := core.Title{Key: core.ShowKey(show), Type: core.TypeShow, Title: "Flaky", Released: now.Add(-30 * 24 * time.Hour)}
	cat := &showCatalog{fakeCatalog: fakeCatalog{titles: map[core.RatingKey]core.Title{showTitle.Key: showTitle}},
		episodes: []core.Title{ep(1, -30*24*time.Hour), ep(2, 2*time.Hour), ep(3, 3*24*time.Hour)}}
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	note := &flakyNotifier{}
	s := New(db, Options{Catalog: cat, Resolver: &episodeResolver{missing: map[core.RatingKey]bool{}}, Notifier: note, Now: func() time.Time { return now }})
	if err := s.SaveSettings(ctx, Settings{Enabled: true, Notify: true, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	if err := s.Remind(ctx, u, showTitle.Key); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, u); err != nil { // available through episode 1
		t.Fatal(err)
	}
	alerts := func() []string { return append([]string(nil), note.titles...) }

	// Episode 2 airs; the alert insert fails inside the recording
	// transaction, so the episode is not recorded either.
	now = now.Add(3 * time.Hour)
	if _, err := db.Exec(`CREATE TRIGGER fail_alert BEFORE INSERT ON watchlist_episodes_alerts BEGIN SELECT RAISE(ABORT,'disk I/O error'); END`); err != nil {
		t.Skipf("trigger unsupported: %v", err)
	}
	if err := s.Check(ctx, u); err == nil {
		t.Fatal("database failure not reported")
	}
	if known, _ := s.episodeKnown(ctx, u.ID, showTitle.Key, ep(2, 0).Key); known {
		t.Fatal("episode recorded without its alert")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_alert`); err != nil {
		t.Fatal(err)
	}

	// Next due pass: recorded, but queuing the alert fails.
	note.fail = true
	now = now.Add(freshAiringCheck)
	if err := s.Check(ctx, u); err != nil {
		t.Fatal(err)
	}
	if known, _ := s.episodeKnown(ctx, u.ID, showTitle.Key, ep(2, 0).Key); !known || len(alerts()) != 1 {
		t.Fatalf("known=%v alerts=%q", known, alerts())
	}
	// Retried once the lease lapses, on any later pass.
	note.fail = false
	now = now.Add(alertLease / 2)
	if err := s.Check(ctx, u); err != nil {
		t.Fatal(err)
	}
	if len(alerts()) != 1 {
		t.Fatalf("retried inside the lease: %q", alerts())
	}
	now = now.Add(alertLease)
	for i := 0; i < 3; i++ {
		if err := s.Check(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if got := alerts(); len(got) != 2 || got[1] != "Flaky S01E02" {
		t.Fatalf("alerts %q", got)
	}
}

// onceNotifier queues each key once, like notify.Service.
type onceNotifier struct {
	recordingNotifier
	keys map[string]bool
}

func (n *onceNotifier) TitleAvailableOnce(ctx context.Context, key, title string) error {
	if n.keys[key] {
		return nil
	}
	n.keys[key] = true
	return n.recordingNotifier.TitleAvailable(ctx, title)
}

// The alert was queued but marking it sent failed (or the process died): a
// once-notifier is never asked to queue it twice in effect, and a plain one
// repeats it a bounded number of times.
func TestNewEpisodeAlertQueuedButNotMarkedSent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		notifier  Notifier
		wantMax   int
		wantExact bool
	}{
		{"once notifier", &onceNotifier{keys: map[string]bool{}}, 1, true},
		{"plain notifier", &recordingNotifier{}, 1 + alertAttempts, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
			const show = 777
			ep := func(e int, aired time.Duration) core.Title {
				return core.Title{Key: core.EpisodeKey(show, 1, e), Type: core.TypeEpisode, ShowTitle: "Once", Season: 1, Episode: e, Released: now.Add(aired)}
			}
			showTitle := core.Title{Key: core.ShowKey(show), Type: core.TypeShow, Title: "Once", Released: now.Add(-30 * 24 * time.Hour)}
			cat := &showCatalog{fakeCatalog: fakeCatalog{titles: map[core.RatingKey]core.Title{showTitle.Key: showTitle}},
				episodes: []core.Title{ep(1, -30*24*time.Hour), ep(2, 2*time.Hour)}}
			db, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			s := New(db, Options{Catalog: cat, Resolver: &episodeResolver{missing: map[core.RatingKey]bool{}}, Notifier: tc.notifier, Now: func() time.Time { return now }})
			if err := s.SaveSettings(ctx, Settings{Enabled: true, Notify: true, CheckHours: 6}); err != nil {
				t.Fatal(err)
			}
			u := &core.User{ID: 1}
			if err := s.Remind(ctx, u, showTitle.Key); err != nil {
				t.Fatal(err)
			}
			if err := s.Check(ctx, u); err != nil {
				t.Fatal(err)
			}
			base := 1 // the show's own availability alert
			if _, err := db.Exec(`CREATE TRIGGER fail_sent BEFORE UPDATE OF sent ON watchlist_episodes_alerts WHEN NEW.sent=1 BEGIN SELECT RAISE(ABORT,'database is locked'); END`); err != nil {
				t.Skipf("trigger unsupported: %v", err)
			}
			now = now.Add(3 * time.Hour)
			if err := s.Check(ctx, u); err == nil {
				t.Fatal("failure to mark the alert sent not reported")
			}
			for i := 0; i < 2*alertAttempts; i++ { // many later passes
				now = now.Add(alertLease + time.Second)
				_ = s.Check(ctx, u)
			}
			if _, err := db.Exec(`DROP TRIGGER fail_sent`); err != nil {
				t.Fatal(err)
			}
			now = now.Add(alertLease + time.Second)
			if err := s.Check(ctx, u); err != nil {
				t.Fatal(err)
			}
			var titles []string
			switch n := tc.notifier.(type) {
			case *onceNotifier:
				titles = n.titles
			case *recordingNotifier:
				titles = n.titles
			}
			got := len(titles) - base
			if got < 1 || got > tc.wantMax || (tc.wantExact && got != tc.wantMax) {
				t.Fatalf("episode alert queued %d times, want <=%d (exact=%v)", got, tc.wantMax, tc.wantExact)
			}
		})
	}
}
