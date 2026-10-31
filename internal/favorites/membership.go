package favorites

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Membership reads one currently authorized favorite without scanning a list.
// AddedAt is the stored insertion time. It is nil for a stored record without
// a known positive timestamp, and it is not the title's date added.
func (s *Service) Membership(ctx context.Context, viewer *core.User, subject Subject) (bool, *time.Time, error) {
	if err := s.authorize(ctx, viewer, subject); err != nil {
		return false, nil, err
	}
	var added int64
	err := s.o.DB.QueryRowContext(ctx, `SELECT added_at FROM favorites_items WHERE viewer_id=? AND kind=? AND subject_id=?`, viewer.ID, string(subject.Kind), subject.ID).Scan(&added)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if added <= 0 {
		return true, nil, nil
	}
	at := time.Unix(0, added).UTC()
	return true, &at, nil
}
