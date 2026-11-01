package availability

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// TitleEvidence is why a movie is shown or hidden. Codes are for the owner
// console; they are not sentences.
type TitleEvidence struct {
	Code      string `json:"code"`
	CheckedAt int64  `json:"checkedAt,omitempty"`
	NextCheck int64  `json:"nextCheck,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Service) Explain(ctx context.Context, key core.RatingKey) (TitleEvidence, error) {
	t, err := s.d.Catalog.CachedTitle(ctx, key)
	if err != nil || t == nil {
		return TitleEvidence{Code: "unknown"}, err
	}
	cfg := s.cfg().Availability
	if !evidenceWindow(*t, s.now(), cfg) {
		return TitleEvidence{Code: "outside"}, nil
	}
	visible := len(s.FilterDiscovery(ctx, []core.Title{*t})) == 1
	var checked, next int64
	var state, reason string
	qerr := s.db.QueryRowContext(ctx, `SELECT checked_at,next_check,state,reason FROM availability_title_checks WHERE rating_key=?`, int64(key)).Scan(&checked, &next, &state, &reason)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return TitleEvidence{}, qerr
	}
	out := TitleEvidence{CheckedAt: checked, NextCheck: next, Reason: reason}
	if visible {
		if state == "found" || s.hasPlayableObservation(ctx, key) {
			out.Code = "playable"
			return out, nil
		}
		out.Code = "digital"
		return out, nil
	}
	if state == "quality" {
		out.Code = "quality"
		return out, nil
	}
	if s.theatricalOnly(ctx, key) {
		out.Code = "theatrical"
		return out, nil
	}
	out.Code = "searching"
	return out, nil
}

func (s *Service) hasPlayableObservation(ctx context.Context, key core.RatingKey) bool {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM availability_releases WHERE rating_key=? AND state<>'unavailable' AND state<>'policy'`, int64(key)).Scan(&n)
	return err == nil && n > 0
}

func (s *Service) theatricalOnly(ctx context.Context, key core.RatingKey) bool {
	h, ok := s.d.Catalog.(interface {
		HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
	})
	if !ok {
		return false
	}
	got, err := h.HomeReleases(ctx, []core.RatingKey{key})
	if err != nil {
		return false
	}
	at, known := got[key]
	return known && at.IsZero()
}
