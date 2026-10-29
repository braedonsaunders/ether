package identity

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("identity_viewer_times", "auth")
	store.Register("identity_viewer_times", 1, `CREATE TABLE identity_viewer_times (
 viewer_id INTEGER PRIMARY KEY REFERENCES auth_users(id) ON DELETE CASCADE,
 last_login INTEGER NOT NULL DEFAULT 0,
 last_activity INTEGER NOT NULL DEFAULT 0
 );`)
}

const activityInterval = 5 * time.Minute

// ViewerTimes stores observed events, rather than inferring them from grants.
// Its activity throttle is local; the database update is monotonic across nodes.
type ViewerTimes struct {
	db     *sql.DB
	now    func() time.Time
	mu     sync.Mutex
	recent map[int64]int64
}

func NewViewerTimes(db *sql.DB, now func() time.Time) (*ViewerTimes, error) {
	if db == nil {
		return nil, errors.New("identity: viewer times requires database")
	}
	if now == nil {
		now = time.Now
	}
	return &ViewerTimes{db: db, now: now, recent: make(map[int64]int64)}, nil
}

func recordLogin(ctx context.Context, tx *sql.Tx, viewerID, at int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO identity_viewer_times(viewer_id,last_login,last_activity) VALUES(?,?,?)
 ON CONFLICT(viewer_id) DO UPDATE SET
 last_login=CASE WHEN excluded.last_login>identity_viewer_times.last_login THEN excluded.last_login ELSE identity_viewer_times.last_login END,
 last_activity=CASE WHEN excluded.last_activity>identity_viewer_times.last_activity THEN excluded.last_activity ELSE identity_viewer_times.last_activity END`, viewerID, at, at)
	return err
}

// Observe records an authenticated request at most once per viewer per node
// per interval. Failed writes are not cached; an older node cannot regress data.
func (s *ViewerTimes) Observe(ctx context.Context, viewerID int64) error {
	if viewerID <= 0 {
		return errors.New("identity: invalid viewer")
	}
	at := s.now().UnixMilli()
	if at <= 0 {
		return errors.New("identity: invalid observation time")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if last := s.recent[viewerID]; last > 0 && (at < last || at-last < activityInterval.Milliseconds()) {
		return nil
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO identity_viewer_times(viewer_id,last_activity) VALUES(?,?)
 ON CONFLICT(viewer_id) DO UPDATE SET last_activity=excluded.last_activity
 WHERE excluded.last_activity>=identity_viewer_times.last_activity+?`, viewerID, at, activityInterval.Milliseconds())
	if err != nil {
		return err
	}
	if rows, e := result.RowsAffected(); e != nil {
		return e
	} else if rows == 0 {
		if err = s.db.QueryRowContext(ctx, `SELECT last_activity FROM identity_viewer_times WHERE viewer_id=?`, viewerID).Scan(&at); err != nil {
			return err
		}
	}
	if len(s.recent) >= 4096 {
		s.recent = make(map[int64]int64)
	}
	s.recent[viewerID] = at
	return nil
}

// Get authorizes the active actor and active target using shared database
// state, so a stale node cannot assert owner access. Plex's durable last_seen
// records real authenticated events and can contribute to the activity time.
func (s *ViewerTimes) Get(ctx context.Context, actorID, viewerID int64) (frontend.ViewerTimes, error) {
	var login, activity, plexSeen int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(t.last_login,0),COALESCE(t.last_activity,0),u.last_seen_at
 FROM auth_users u LEFT JOIN identity_viewer_times t ON t.viewer_id=u.id
 WHERE u.id=? AND u.active=1 AND EXISTS (SELECT 1 FROM auth_users a WHERE a.id=? AND a.active=1 AND (a.id=u.id OR a.is_owner=1))`, viewerID, actorID).Scan(&login, &activity, &plexSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return frontend.ViewerTimes{}, core.ErrUnauthorized
	}
	if err != nil {
		return frontend.ViewerTimes{}, err
	}
	if plexSeen > 0 && plexSeen <= (1<<63-1)/1000 && plexSeen*1000 > activity {
		activity = plexSeen * 1000
	}
	var result frontend.ViewerTimes
	if login > 0 {
		result.LastLogin = time.UnixMilli(login).UTC()
	}
	if activity > 0 {
		result.LastActivity = time.UnixMilli(activity).UTC()
	}
	return result, nil
}
