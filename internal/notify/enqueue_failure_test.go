package notify

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/usenet"
)

func TestOutageAlertsRetryAfterQueueFailure(t *testing.T) {
	for _, kind := range []string{"provider", "indexer"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			now := time.Now()
			s := New(db, Options{Now: func() time.Time { return now }, Providers: func() []usenet.ProviderStats { return []usenet.ProviderStats{{ID: "p", State: usenet.StateBackoff}} }})
			if err := s.Save(ctx, Channel{ID: "hook", Kind: "webhook", Enabled: true, Endpoint: "https://fixture.example/hook", Events: []string{"*"}}); err != nil {
				t.Fatal(err)
			}
			if store.DialectOf(db) == store.Postgres {
				t.Skip("the queue failure is injected with a SQLite trigger")
			}
			if _, err := db.Exec(`CREATE TRIGGER reject_notification BEFORE INSERT ON notify_queue BEGIN SELECT RAISE(ABORT,'temporary failure'); END`); err != nil {
				t.Fatal(err)
			}
			observe := func() {
				if kind == "provider" {
					_ = s.SampleHealth(ctx)
				} else {
					s.ObserveIndexer(newznab.Event{Indexer: "fixture", Kind: "search"})
				}
			}
			observe()
			if _, err := db.Exec(`DROP TRIGGER reject_notification`); err != nil {
				t.Fatal(err)
			}
			if kind == "indexer" {
				observe()
				var queued int
				if err := db.QueryRow(`SELECT COUNT(*) FROM notify_queue`).Scan(&queued); err != nil {
					t.Fatal(err)
				}
				if queued != 0 {
					t.Fatal("failed queue retried without backoff")
				}
			}
			now = now.Add(time.Minute)
			observe()
			observe()
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM notify_queue`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("queued %d alerts after recovery, want exactly one", count)
			}
		})
	}
}

func TestEmitQueuesAllChannelsAtomically(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	for _, id := range []string{"a", "b"} {
		if err := s.Save(ctx, Channel{ID: id, Kind: "webhook", Enabled: true, Endpoint: "https://fixture.example/hook", Events: []string{"*"}}); err != nil {
			t.Fatal(err)
		}
	}
	if store.DialectOf(db) == store.Postgres {
		t.Skip("the queue failure is injected with a SQLite trigger")
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_notification BEFORE INSERT ON notify_queue WHEN NEW.channel='b' BEGIN SELECT RAISE(ABORT,'temporary failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(ctx, Event{Kind: "test"}); err == nil {
		t.Fatal("queue failure hidden")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notify_queue`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partial queue write: %d entries", count)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_notification`); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(ctx, Event{Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM notify_queue`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("retry queued %d entries", count)
	}
}
