package watchlist

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/store"
)

var _ OnceNotifier = (*notify.Service)(nil)

// Exercise the real notifier and persistent queue across a service restart,
// including the gap between successful enqueue and the watchlist sent marker.
// No delivery worker is started and no external endpoint is contacted.
func TestEpisodeAlertRealQueueSurvivesCompletionFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	const showID = 765
	show := core.Title{Key: core.ShowKey(showID), Type: core.TypeShow, Title: "Integration", Released: now.Add(-24 * time.Hour)}
	episode := core.Title{Key: core.EpisodeKey(showID, 1, 2), Type: core.TypeEpisode, ShowTitle: show.Title, Season: 1, Episode: 2, Released: now.Add(-time.Hour)}
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	notifier := notify.New(db, notify.Options{Now: clock, NodeID: "first"})
	if err := notifier.Save(ctx, notify.Channel{ID: "alerts", Kind: "webhook", Enabled: true, Endpoint: "https://notifications.invalid/hook", Events: []string{"title.available"}, PerMinute: 60}); err != nil {
		t.Fatal(err)
	}
	service := func(n *notify.Service) *Service {
		return New(db, Options{Resolver: &episodeResolver{missing: map[core.RatingKey]bool{}}, Notifier: n, Now: clock})
	}
	s := service(notifier)
	if _, err := db.Exec(`CREATE TRIGGER fail_episode_sent BEFORE UPDATE OF sent ON watchlist_episodes_alerts WHEN NEW.sent=1 BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.newEpisode(ctx, Settings{Notify: true}, &core.User{ID: 1}, &show, &episode); err == nil {
		t.Fatal("completion failure was not surfaced")
	}
	assertQueue := func(wantSent int) {
		t.Helper()
		var queued, sent int
		if err := db.QueryRow(`SELECT COUNT(*) FROM notify_queue`).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT sent FROM watchlist_episodes_alerts WHERE episode_key=?`, episode.Key).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		if queued != 1 || sent != wantSent {
			t.Fatalf("queued=%d sent=%d, want 1 and %d", queued, sent, wantSent)
		}
	}
	assertQueue(0)
	if _, err := db.Exec(`DROP TRIGGER fail_episode_sent`); err != nil {
		t.Fatal(err)
	}
	// Fresh services have no memory of the successful enqueue. The durable
	// notify_once key must absorb the retry and let the sent marker complete.
	s = service(notify.New(db, notify.Options{Now: clock, NodeID: "restarted"}))
	now = now.Add(alertLease + time.Second)
	if err := s.retryAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	assertQueue(1)
	if err := s.retryAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	assertQueue(1)
}
