package watchlist

import (
	"context"
	"database/sql"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Membership reads one viewer/title pair across current watchlist sources.
// The latest stored source timestamp matches Items' existing ordering. It is
// an observation or import time, not the title's date added and not
// necessarily the upstream addition time. A zero timestamp is unknown rather
// than the Unix epoch.
func (s *Service) Membership(ctx context.Context, userID int64, key core.RatingKey) (bool, *time.Time, error) {
	if userID <= 0 || !key.Valid() {
		return false, nil, core.ErrUnauthorized
	}
	var count int
	var added sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(added) FROM watchlist_items WHERE user_id=? AND item_key=?`, userID, key).Scan(&count, &added)
	if err != nil {
		return false, nil, err
	}
	if count == 0 {
		return false, nil, nil
	}
	if !added.Valid || added.Int64 <= 0 {
		return true, nil, nil
	}
	at := time.Unix(added.Int64, 0).UTC()
	return true, &at, nil
}
