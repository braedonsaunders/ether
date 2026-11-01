package safety

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// ConfirmPlayback reads membership and safety policy inside one fenced
// transaction. Restricted viewers fail closed when the store is unavailable.
// Unrestricted viewers return ErrPolicyUnconfirmed so cached playback can continue.
// An unrestricted viewer whose epoch has not moved since the last fence is
// answered from that snapshot, so a play does not take a write lock.
func (s *Service) ConfirmPlayback(ctx context.Context, u *core.User, remote bool, now time.Time) (*core.User, error) {
	if s == nil || s.db == nil || u == nil || u.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	restricted := s.Restricted(u)
	if !restricted {
		epoch, err := viewerpolicy.Read(ctx, s.db)
		if err == nil {
			if merged, policy, ok := s.cachedConfirm(u.ID, epoch); ok {
				if !CanPlayWithPolicy(merged, policy, remote, now) {
					return nil, core.ErrUnauthorized
				}
				return merged, nil
			}
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		epoch, err := viewerpolicy.Read(ctx, s.db)
		if err != nil {
			return confirmFallback(restricted)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return confirmFallback(restricted)
		}
		started := time.Now()
		err = viewerpolicy.Fence(ctx, s.db, tx, epoch)
		if errors.Is(err, viewerpolicy.ErrChanged) {
			_ = tx.Rollback()
			continue
		}
		if err != nil {
			_ = tx.Rollback()
			return confirmFallback(restricted)
		}
		merged, policy, err := readFreshPlayback(ctx, tx, u)
		if errors.Is(err, core.ErrUnauthorized) {
			_ = tx.Rollback()
			return nil, err
		}
		if err != nil {
			_ = tx.Rollback()
			return confirmFallback(restricted)
		}
		if err := tx.Commit(); err != nil {
			return confirmFallback(restricted)
		}
		ended := time.Now()
		if !CanPlayWithPolicy(merged, policy, remote, started) || !CanPlayWithPolicy(merged, policy, remote, ended) || !CanPlayWithPolicy(merged, policy, remote, now) {
			return nil, core.ErrUnauthorized
		}
		if !s.Restricted(merged) {
			s.storeConfirm(merged.ID, epoch, merged, policy)
		}
		return merged, nil
	}
	return confirmFallback(restricted)
}

func (s *Service) cachedConfirm(id, epoch int64) (*core.User, Policy, bool) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	hit, ok := s.playbackEpoch[id]
	if !ok || hit.epoch != epoch {
		return nil, Policy{}, false
	}
	user := hit.user
	user.AllowedSections = append([]int(nil), hit.user.AllowedSections...)
	return &user, hit.policy, true
}

func (s *Service) storeConfirm(id, epoch int64, user *core.User, policy Policy) {
	if user == nil {
		return
	}
	stored := *user
	stored.AllowedSections = append([]int(nil), user.AllowedSections...)
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if s.playbackEpoch == nil {
		s.playbackEpoch = map[int64]playbackConfirm{}
	}
	s.playbackEpoch[id] = playbackConfirm{epoch: epoch, user: stored, policy: policy}
}

func confirmFallback(restricted bool) (*core.User, error) {
	if restricted {
		return nil, core.ErrUnauthorized
	}
	return nil, core.ErrPolicyUnconfirmed
}

func readFreshPlayback(ctx context.Context, tx *sql.Tx, u *core.User) (*core.User, Policy, error) {
	var active, owner int
	var rating, allowed, plexSecs, raw string
	err := tx.QueryRowContext(ctx, `SELECT active, is_owner, max_content_rating, allowed_sections, plex_sections, COALESCE((SELECT policy FROM safety_policies WHERE user_id=auth_users.id), '') FROM auth_users WHERE id=?`, u.ID).Scan(&active, &owner, &rating, &allowed, &plexSecs, &raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && active != 1) {
		return nil, Policy{}, core.ErrUnauthorized
	}
	if err != nil {
		return nil, Policy{}, err
	}
	var policy Policy
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return nil, Policy{}, err
		}
		if err := policy.Validate(); err != nil {
			return nil, Policy{}, err
		}
	}
	out := *u
	out.IsOwner = owner == 1
	out.MaxContentRating = rating
	var sections, plex []int
	_ = json.Unmarshal([]byte(allowed), &sections)
	_ = json.Unmarshal([]byte(plexSecs), &plex)
	if len(sections) == 0 && len(plex) > 0 && !out.IsOwner {
		sections = plex
	}
	out.AllowedSections = append([]int(nil), sections...)
	if policy.MaxContentRating != "" && (out.MaxContentRating == "" || int(contentrating.LevelOf(policy.MaxContentRating)) < int(contentrating.LevelOf(out.MaxContentRating))) {
		out.MaxContentRating = policy.MaxContentRating
	}
	if policy.StreamLimit > 0 && (out.StreamLimit <= 0 || policy.StreamLimit < out.StreamLimit) {
		out.StreamLimit = policy.StreamLimit
	}
	return &out, policy, nil
}
