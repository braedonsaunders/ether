package app

import (
	"errors"
	"io"
	"log/slog"
	"testing"
)

func TestBackupAlertsOncePerStreakAndRetryUnqueued(t *testing.T) {
	var sent int
	queueDown := true
	b := &backupAlerts{log: slog.New(slog.NewTextHandler(io.Discard, nil)), notify: func(error) error {
		if queueDown {
			return errors.New("database is locked")
		}
		sent++
		return nil
	}}
	fail := errors.New("no space left on device")
	b.observe(fail) // alert not queued: must be retried
	queueDown = false
	b.observe(fail)
	b.observe(fail)
	if sent != 1 {
		t.Fatalf("sent %d alerts for one streak, want 1", sent)
	}
	b.observe(nil)
	b.observe(fail)
	if sent != 2 {
		t.Fatalf("sent %d, want a new alert after recovery", sent)
	}
}
