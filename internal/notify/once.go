package notify

import (
	"context"
	"fmt"

	"github.com/braedonsaunders/ether/internal/store"
)

// One-time events are recorded by key so an alert about one fact (an update
// to a given version) goes out once, whichever node sees it first and however
// often the process restarts. Its own migration component keeps the numbering
// independent of the notify component's.
func init() {
	store.Register("notify_once", 1, `CREATE TABLE notify_once(key TEXT PRIMARY KEY, at INTEGER NOT NULL);`)
}

// EmitOnce emits e unless an event was already emitted under key, and
// reports whether it emitted. The key and the queued deliveries commit in one
// transaction: a crash or a failed enqueue leaves neither, so a later attempt
// emits it.
func (s *Service) EmitOnce(ctx context.Context, key string, e Event) (bool, error) {
	e, selected, err := s.prepare(ctx, e)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO notify_once(key,at) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, key, s.opts.Now().Unix())
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	for _, c := range selected {
		if err := enqueueWith(ctx, tx, c, e, s.opts.Now()); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// UpdateAvailable announces a release once per version.
func (s *Service) UpdateAvailable(ctx context.Context, current, version string) error {
	_, err := s.EmitOnce(ctx, "update.available:"+version, Event{
		Kind:    "update.available",
		Title:   "Ether " + version + " is available",
		Message: fmt.Sprintf("This server runs %s. Open Maintenance → Updates to install %s.", current, version),
	})
	return err
}

// TitleAvailableOnce publishes availability once per key (an episode, say),
// however often a caller retries after a failure or crash.
func (s *Service) TitleAvailableOnce(ctx context.Context, key, title string) error {
	_, err := s.EmitOnce(ctx, "title.available:"+key, Event{Kind: "title.available", Title: "Title available", Message: title})
	return err
}

// BackupFailed reports a scheduled backup that did not complete.
func (s *Service) BackupFailed(ctx context.Context, err error) error {
	return s.Emit(ctx, Event{
		Kind:    "backup.failed",
		Title:   "Scheduled backup failed",
		Message: "Ether could not take its scheduled backup: " + err.Error(),
	})
}
