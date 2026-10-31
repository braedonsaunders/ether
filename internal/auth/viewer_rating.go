package auth

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

// SetViewerMaxRating changes only the content ceiling without overwriting other viewer settings.
func (s *Service) SetViewerMaxRating(ctx context.Context, id int64, rating string) error {
	rating = strings.ToUpper(strings.TrimSpace(rating))
	if rating != "" && catalog.RatingRank(rating) == 0 {
		return core.ErrNotAvailable
	}
	res, err := s.db.ExecContext(ctx, `UPDATE auth_users SET max_content_rating=?,updated_at=? WHERE id=? AND active=1 AND is_owner=1`, rating, s.o.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return core.ErrUnauthorized
	}
	s.gen.Add(1)
	if err = s.reload(ctx); err != nil {
		return err
	}
	s.notify("")
	return nil
}
