package livetv

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("livetv", 15, `CREATE TABLE livetv_refresh_leases(id TEXT PRIMARY KEY, holder TEXT NOT NULL, until_ms BIGINT NOT NULL);`)
}

const clientIdleWindow = 5 * time.Minute

// NoteClientActivity is called only after client authentication. It does no
// database or provider work. The joined refresh loop coalesces wakeups and serves
// requests from the last imported snapshot while an update is in progress.
func (s *Service) NoteClientActivity() {
	now := s.o.Now()
	s.mu.Lock()
	idle := (s.lastClientActivity.IsZero() || now.Sub(s.lastClientActivity) >= clientIdleWindow) && len(s.sessions) == 0
	s.lastClientActivity = now
	s.mu.Unlock()
	if idle && s.serviceContext().Err() == nil {
		select {
		case s.activityWake <- struct{}{}:
		default:
		}
	}
}

func activityRefreshDue(src Source, now time.Time) bool {
	if _, enabled := refreshInterval(src); !enabled || src.ActivityRefreshMinutes <= 0 {
		return false
	}
	cooldown := time.Duration(max(30, src.ActivityRefreshMinutes)) * time.Minute
	next := src.UpdatedAt.Add(cooldown)
	if src.LastErrorAt != nil && src.LastErrorAt.After(src.UpdatedAt) {
		if retry, ok := nextRefresh(src, nil); ok && retry.After(next) {
			next = retry
		}
		if retry := src.LastErrorAt.Add(cooldown); retry.After(next) {
			next = retry
		}
	}
	return !now.Before(next)
}

// acquireRefresh admits one upstream refresh across nodes without holding a
// database transaction open during network I/O. Expiry recovers crashed owners.
func (s *Service) acquireRefresh(ctx context.Context, id string) (func(), error) {
	now, holder := s.o.Now(), randomID()
	result, err := s.o.DB.ExecContext(ctx, `INSERT INTO livetv_refresh_leases(id,holder,until_ms) VALUES(?,?,?)
ON CONFLICT(id) DO UPDATE SET holder=excluded.holder,until_ms=excluded.until_ms WHERE livetv_refresh_leases.until_ms<=?`, id, holder, now.Add(refreshBudget+time.Minute).UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, core.ErrNotAvailable
	}
	return func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.o.DB.ExecContext(cleanup, `DELETE FROM livetv_refresh_leases WHERE id=? AND holder=?`, id, holder)
	}, nil
}
