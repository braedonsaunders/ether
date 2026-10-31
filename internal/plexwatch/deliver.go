package plexwatch

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/state"
)

const missingGUIDLimit = 8

// Run delivers until Close or ctx ends. It does not decide cluster leadership:
// row claims are safe for every node.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		s.deliverAvailable(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-tick.C:
		}
	}
}

func (s *Service) deliverAvailable(ctx context.Context) {
	if s.lastCompact.IsZero() || s.now().Sub(s.lastCompact) >= time.Hour {
		s.lastCompact = s.now()
		if err := s.state.CompactSync(ctx, s.now()); err != nil && ctx.Err() == nil {
			s.log.Warn("plex watch journal compact", "err", err.Error())
		}
		if err := s.compactSent(ctx, sentPerUserCap); err != nil && ctx.Err() == nil {
			s.log.Warn("plex watch echo compact", "err", err.Error())
		}
	}
	if _, err := s.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("plex watch hold reconcile", "err", err.Error())
	}
	for {
		more, err := s.DeliverOnce(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("plex watch delivery paused", "err", err.Error())
		}
		if !more || err != nil || ctx.Err() != nil {
			return
		}
	}
}

// DeliverOnce sends at most one live intent. more is true when a row was
// claimed, including rows skipped because a newer intent replaced them.
func (s *Service) DeliverOnce(ctx context.Context) (bool, error) {
	now := s.now()
	attempt := s.node + ":" + newID()
	row, ok, err := s.state.ClaimSync(ctx, attempt, now, s.claimFor)
	if err != nil || !ok {
		return false, err
	}
	locked, err := s.state.LockSync(ctx, row.UserID, row.Key, row.Aspect, attempt, now, s.claimFor)
	if err != nil {
		return true, err
	}
	if !locked {
		if err := s.state.PauseSync(ctx, row.Seq, attempt, now.Add(time.Second), "contended"); err != nil {
			return true, err
		}
		return true, nil
	}
	defer s.state.UnlockSync(ctx, row.UserID, row.Key, row.Aspect, attempt)
	if s.attemptStart != nil {
		s.attemptStart(attempt)
	}
	return true, s.deliverClaim(ctx, row, attempt)
}

func (s *Service) deliverClaim(ctx context.Context, row state.SyncRow, attempt string) error {
	current, err := s.state.SyncCurrent(ctx, row.Seq, attempt)
	if err != nil || !current {
		return err
	}
	acct, ok, err := s.loadAccount(ctx, row.UserID)
	if err != nil {
		return err
	}
	if !ok || acct.revoked || acct.token == "" || !acct.enabled || acct.plexID <= 0 {
		return s.state.DeadSync(ctx, row.Seq, attempt, "disabled")
	}
	if acct.eligibility != "allowed" {
		return s.state.DeferSync(ctx, row.Seq, attempt, s.now().Add(15*time.Minute), "not_eligible")
	}
	if row.Kind == state.SyncRatingKind {
		if _, ok := starRating(row.Rating); !ok {
			return s.state.DeadSync(ctx, row.Seq, attempt, "rating_not_sent")
		}
	}
	if row.Key.Kind() != core.KindMovie && row.Key.Kind() != core.KindEpisode {
		return s.state.DeadSync(ctx, row.Seq, attempt, "guid_kind")
	}
	guid, gerr := s.lookupGUID(ctx, row.Key)
	if gerr != nil {
		if errors.Is(gerr, plexguid.ErrAmbiguous) || errors.Is(gerr, errGUIDKind) {
			return s.state.DeadSync(ctx, row.Seq, attempt, "ambiguous_guid")
		}
		if row.Attempts+1 >= missingGUIDLimit {
			return s.state.DeadSync(ctx, row.Seq, attempt, "missing_guid")
		}
		return s.state.DeferSync(ctx, row.Seq, attempt, s.now().Add(backoff(row.Attempts)), "missing_guid")
	}
	at := row.At
	if at.IsZero() {
		at = s.now()
	}
	if at.After(s.now().Add(2 * time.Minute)) {
		return s.state.DeferSync(ctx, row.Seq, attempt, at, "future_time")
	}
	item := postItem{GUID: guid}
	sentKind := row.Kind
	sentAt := at.UnixMilli()
	sentRating := 0
	switch row.Kind {
	case state.SyncComplete, state.SyncUnplayed:
		item.ViewedAt = sentAt
		item.ViewState = row.Kind
		item.IsBulk = row.Bulk
	case state.SyncRatingKind:
		n, _ := starRating(row.Rating)
		item.RatedAt = sentAt
		item.Rating = &n
		sentRating = n
		sentKind = "rating"
	default:
		return s.state.DeadSync(ctx, row.Seq, attempt, "rejected")
	}
	permit, err := s.acquireSend(ctx, row, attempt, item)
	if err != nil {
		if rerr := s.state.ReleaseSync(ctx, row.Seq, attempt); rerr != nil {
			return rerr
		}
		return err
	}
	if !permit.OK {
		current, err = s.state.SyncCurrent(ctx, row.Seq, attempt)
		if err != nil || !current {
			return err
		}
		if permit.Terminal == "not_eligible" {
			return s.state.DeferSync(ctx, row.Seq, attempt, s.now().Add(15*time.Minute), "not_eligible")
		}
		reason := permit.Terminal
		if reason == "" {
			reason = "consent"
		}
		return s.state.DeadSync(ctx, row.Seq, attempt, reason)
	}
	defer s.releaseSend(ctx, permit.ID)
	if s.afterAdmission != nil {
		if err := s.afterAdmission(); err != nil {
			return err
		}
	}
	err = s.client.postState(ctx, permit.Token, permit.PlexID, item)
	if err != nil {
		return s.failDelivery(ctx, row, attempt, permit.ID, permit.Generation, err)
	}
	if s.afterPost != nil {
		if err := s.afterPost(); err != nil {
			return err
		}
	}
	return s.completeSend(ctx, row, permit.ID, guid, sentKind, sentAt, sentRating)
}

var errGUIDKind = errors.New("plexwatch: guid kind does not match the title")

func (s *Service) lookupGUID(ctx context.Context, key core.RatingKey) (string, error) {
	if s.guid == nil {
		return "", core.ErrNotFound
	}
	guid, err := s.guid.Resolve(ctx, key)
	if err != nil {
		return "", err
	}
	switch key.Kind() {
	case core.KindMovie:
		if !strings.HasPrefix(guid, "plex://movie/") {
			return "", errGUIDKind
		}
	case core.KindEpisode:
		if !strings.HasPrefix(guid, "plex://episode/") {
			return "", errGUIDKind
		}
	default:
		return "", errGUIDKind
	}
	return guid, nil
}

func (s *Service) failDelivery(ctx context.Context, row state.SyncRow, attempt, permitID string, generation int64, err error) error {
	var st *StatusError
	if errors.As(err, &st) {
		switch {
		case st.Status == 401:
			s.log.Info("plex watch token rejected", "seq", row.Seq, "generation", generation)
			if cerr := s.clearAttempt(ctx, permitID, row.Seq, false); cerr != nil {
				return cerr
			}
			rerr := s.revoke(ctx, row.UserID, generation)
			if rerr == nil {
				return s.state.DeadSync(ctx, row.Seq, attempt, "unauthorized")
			}
			if !errors.Is(rerr, ErrRevoked) {
				return rerr
			}
			return nil
		case st.Status == 403:
			if cerr := s.clearAttempt(ctx, permitID, row.Seq, true); cerr != nil {
				return cerr
			}
			_, _ = s.db.ExecContext(ctx, `UPDATE plexwatch_account SET eligibility='refused', enabled=0, checked_at=? WHERE user_id=? AND token_generation=?`,
				s.now().UnixNano(), row.UserID, generation)
			return s.state.DeferSync(ctx, row.Seq, attempt, s.now().Add(15*time.Minute), "not_eligible")
		case st.Status == 400 || st.Status == 422:
			s.log.Info("plex watch state rejected", "seq", row.Seq, "status", st.Status)
			if cerr := s.clearAttempt(ctx, permitID, row.Seq, false); cerr != nil {
				return cerr
			}
			return s.state.DeadSync(ctx, row.Seq, attempt, "rejected")
		}
	}
	return s.state.DeadSync(ctx, row.Seq, attempt, "uncertain_send")
}

func backoff(attempts int) time.Duration {
	d := time.Second
	for i := 0; i < attempts && d < 15*time.Minute; i++ {
		d *= 2
	}
	if d > 15*time.Minute {
		return 15 * time.Minute
	}
	return d
}
