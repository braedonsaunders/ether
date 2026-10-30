package notify

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

func queued(t *testing.T, s *Service, kind string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notify_queue WHERE event LIKE ?`, `%"kind":"`+kind+`"%`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUpdateAvailableOncePerVersionAcrossNodesAndRestarts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ether.db")
	open := func(id string) *Service {
		db, err := store.OpenPath(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return New(db, Options{NodeID: id})
	}
	a, b := open("a"), open("b")
	if err := a.Save(ctx, Channel{ID: "hook", Kind: "webhook", Enabled: true, Endpoint: "https://hooks.example/x", Events: []string{"update.available"}, PerMinute: 60}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Service{a, b, a, open("a-restarted")} {
		if err := s.UpdateAvailable(ctx, "0.2.47", "0.2.48"); err != nil {
			t.Fatal(err)
		}
	}
	if n := queued(t, a, "update.available"); n != 1 {
		t.Fatalf("0.2.48 announced %d times", n)
	}
	if err := b.UpdateAvailable(ctx, "0.2.47", "0.2.49"); err != nil {
		t.Fatal(err)
	}
	if n := queued(t, a, "update.available"); n != 2 {
		t.Fatalf("a newer version must be announced: %d", n)
	}
}

func TestEmitOnceKeyAndQueueCommitTogether(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	if err := s.Save(ctx, Channel{ID: "hook", Kind: "webhook", Enabled: true, Endpoint: "https://hooks.example/x", Events: []string{"*"}, PerMinute: 60}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.EmitOnce(ctx, "k", Event{Kind: "no.such.kind"}); ok || err == nil {
		t.Fatalf("bad event: %v %v", ok, err)
	}
	// The queue insert fails after the key insert: neither may remain.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_enqueue BEFORE INSERT ON notify_queue BEGIN SELECT RAISE(ABORT,'disk I/O error'); END`); err != nil {
		t.Skipf("trigger unsupported: %v", err)
	}
	if ok, err := s.EmitOnce(ctx, "k", Event{Kind: "backup.failed", Message: "x"}); ok || err == nil {
		t.Fatalf("failed enqueue reported %v %v", ok, err)
	}
	var keys int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notify_once`).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("key kept after failed enqueue: %d %v", keys, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_enqueue`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.EmitOnce(ctx, "k", Event{Kind: "backup.failed", Message: "x"}); !ok || err != nil {
		t.Fatalf("retry after failure: %v %v", ok, err)
	}
	if ok, err := s.EmitOnce(ctx, "k", Event{Kind: "backup.failed", Message: "x"}); ok || err != nil {
		t.Fatalf("duplicate: %v %v", ok, err)
	}
	if n := queued(t, s, "backup.failed"); n != 1 {
		t.Fatalf("queued %d", n)
	}
}

func TestBackupFailedAndUpdateAvailableAreAutomaticEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	if err := s.Save(ctx, Channel{ID: "hook", Kind: "webhook", Enabled: true, Endpoint: "https://hooks.example/x", Events: []string{"backup.failed"}, PerMinute: 60}); err != nil {
		t.Fatal(err)
	}
	if err := s.BackupFailed(ctx, errors.New("no space left on device")); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT event FROM notify_queue`).Scan(&raw); err != nil || !strings.Contains(raw, "no space left on device") {
		t.Fatalf("queued %q %v", raw, err)
	}
	auto := map[string]bool{}
	for _, e := range eventCatalog() {
		auto[e.Kind] = e.Automatic
	}
	if !auto["backup.failed"] || !auto["update.available"] {
		t.Fatalf("catalog %v", auto)
	}
}

func TestTitleAvailableOnceQueuesOncePerKey(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	if err := s.Save(ctx, Channel{ID: "hook", Kind: "webhook", Enabled: true, Endpoint: "https://hooks.example/x", Events: []string{"title.available"}, PerMinute: 60}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.TitleAvailableOnce(ctx, "episode:1", "The Show S01E03"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TitleAvailableOnce(ctx, "episode:2", "The Show S01E04"); err != nil {
		t.Fatal(err)
	}
	if n := queued(t, s, "title.available"); n != 2 {
		t.Fatalf("queued %d", n)
	}
}
