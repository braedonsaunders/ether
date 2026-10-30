package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

// A failing scheduled backup backs off instead of retrying every minute,
// reports each attempt, and resumes the normal schedule once it succeeds.
func TestScheduledBackupBacksOffAfterFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, e := store.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	cfg := filepath.Join(dir, "config.json") // missing: Create fails
	s := New(db, cfg, filepath.Join(dir, "backups"), 1)
	if e = s.Configure(ctx, Settings{Retain: 3, IntervalHours: 24}); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	var r retryState
	var attempts []time.Duration
	for at := start; at.Before(start.Add(6 * time.Hour)); at = at.Add(time.Minute) {
		tried, err := s.runScheduled(ctx, at, &r)
		if tried {
			if err == nil {
				t.Fatal("backup without config succeeded")
			}
			attempts = append(attempts, at.Sub(start))
		}
	}
	want := []time.Duration{0, 15 * time.Minute, 45 * time.Minute, 105 * time.Minute, 225 * time.Minute}
	if len(attempts) != len(want) {
		t.Fatalf("attempts at %v, want %v", attempts, want)
	}
	for i := range want {
		if attempts[i] != want[i] {
			t.Fatalf("attempts at %v, want %v", attempts, want)
		}
	}

	if e = os.WriteFile(cfg, []byte(`{"server":{"friendlyName":"test"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	at := r.next
	if tried, err := s.runScheduled(ctx, at, &r); !tried || err != nil {
		t.Fatalf("recovery: %v %v", tried, err)
	}
	if r.failures != 0 || !r.next.IsZero() {
		t.Fatalf("retry state kept after success: %+v", r)
	}
	if tried, _ := s.runScheduled(ctx, at.Add(time.Minute), &r); tried {
		t.Fatal("backed up again before the interval elapsed")
	}
}
