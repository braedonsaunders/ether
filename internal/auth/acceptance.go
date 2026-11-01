package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	originDurable uint8 = iota + 1
	originDelegation
	originPositive
	originFresh
	originStale
)

// validation is the singleflight result shared by every waiter of one token.
type validation struct {
	user *core.User
	acc  Acceptance
}

type receipt struct {
	origin   uint8
	hash     string
	viewer   int64
	seq      uint64
	deadline time.Time
}

// Acceptance is minted only by Service.Accept. The zero value is invalid.
// Formatting, JSON and logs show a fixed label so a receipt cannot leak a
// credential digest.
type Acceptance struct {
	id          uint64
	incarnation uint64
	viewer      int64
}

// ViewerID is the viewer named by the acceptance. ReceiptCurrent still
// rechecks that identity against the live credential.
func (a Acceptance) ViewerID() int64 { return a.viewer }

// Valid reports whether Accept minted this value. It does not recheck revocation.
func (a Acceptance) Valid() bool { return a.id != 0 && a.incarnation != 0 }

func (a Acceptance) String() string { return "auth.Acceptance" }

func (a Acceptance) GoString() string { return "auth.Acceptance" }

func (a Acceptance) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "auth.Acceptance") }

func (a Acceptance) MarshalJSON() ([]byte, error) { return []byte(`"auth.Acceptance"`), nil }

// ReceiptCurrent reports whether a still names the credential Accept minted
// and that credential's current local revocation state. It does not ask plex.tv.
func (s *Service) ReceiptCurrent(ctx context.Context, a Acceptance) error {
	if s == nil || a.id == 0 || a.incarnation == 0 || a.incarnation != s.incarnation || a.viewer == 0 {
		return core.ErrUnauthorized
	}
	s.mu.RLock()
	rec, ok := s.receipts[a.id]
	if !ok || rec.viewer != a.viewer {
		s.mu.RUnlock()
		return core.ErrUnauthorized
	}
	now := s.o.Now()
	switch rec.origin {
	case originDurable:
		id, present := s.tokens[rec.hash]
		u := s.activePlexUser(id)
		gen := s.tokenGen[rec.hash]
		s.mu.RUnlock()
		if !present || gen != rec.seq || u == nil || u.ID != rec.viewer {
			return core.ErrUnauthorized
		}
		return nil
	case originPositive, originFresh:
		e, present := s.pos[rec.hash]
		u := s.activePlexUser(e.userID)
		s.mu.RUnlock()
		if !present || e.seq != rec.seq || u == nil || u.ID != rec.viewer || !now.Before(e.expires) || !rec.deadline.Equal(e.expires) {
			return core.ErrUnauthorized
		}
		return nil
	case originStale:
		e, present := s.pos[rec.hash]
		u := s.activePlexUser(e.userID)
		deadline := e.expires.Add(s.o.StaleTTL)
		s.mu.RUnlock()
		if !present || e.seq != rec.seq || u == nil || u.ID != rec.viewer || !now.Before(deadline) || !rec.deadline.Equal(deadline) {
			return core.ErrUnauthorized
		}
		return nil
	case originDelegation:
		hash, viewer, deadline := rec.hash, rec.viewer, rec.deadline
		s.mu.RUnlock()
		var id, exp int64
		err := s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM auth_delegations WHERE token_hash=?`, hash).Scan(&id, &exp)
		if err != nil || id != viewer || !deadline.Equal(time.Unix(exp, 0).UTC()) || !now.Before(deadline) {
			return core.ErrUnauthorized
		}
		u, err := s.PolicyUser(ctx, viewer)
		if err != nil || u == nil || u.ID != viewer {
			return core.ErrUnauthorized
		}
		return nil
	default:
		s.mu.RUnlock()
		return core.ErrUnauthorized
	}
}

// PolicyInTx reads durable membership on an open transaction so a caller can
// fence it with the rest of an authorization snapshot.
func (s *Service) PolicyInTx(ctx context.Context, tx *sql.Tx, id int64) (*core.User, error) {
	if s == nil || tx == nil || id <= 0 {
		return nil, core.ErrUnauthorized
	}
	r, err := scanRow(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth_users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if !r.Active {
		return nil, core.ErrUnauthorized
	}
	u := r.effective()
	return &u, nil
}

// Callers hold s.mu for writing.
func (s *Service) mintLocked(hash string, origin uint8, viewer int64, seq uint64, deadline time.Time) Acceptance {
	if acc, ok := s.findLocked(hash, origin, viewer, seq, deadline); ok {
		return acc
	}
	if origin == originPositive || origin == originFresh {
		if acc, ok := s.findEntryReceipt(hash, seq, deadline); ok && acc.viewer == viewer {
			return acc
		}
	}
	s.receiptSeq++
	id := s.receiptSeq
	s.receipts[id] = receipt{origin: origin, hash: hash, viewer: viewer, seq: seq, deadline: deadline}
	s.idsByHash[hash] = append(s.idsByHash[hash], id)
	return Acceptance{id: id, incarnation: s.incarnation, viewer: viewer}
}

// Callers hold s.mu.
func (s *Service) findLocked(hash string, origin uint8, viewer int64, seq uint64, deadline time.Time) (Acceptance, bool) {
	for _, id := range s.idsByHash[hash] {
		rec := s.receipts[id]
		if rec.origin == origin && rec.viewer == viewer && rec.seq == seq && rec.deadline.Equal(deadline) {
			return Acceptance{id: id, incarnation: s.incarnation, viewer: viewer}, true
		}
	}
	return Acceptance{}, false
}

// findEntryReceipt returns the positive or validated receipt for this cache entry.
// Callers hold s.mu.
func (s *Service) findEntryReceipt(hash string, seq uint64, deadline time.Time) (Acceptance, bool) {
	for _, id := range s.idsByHash[hash] {
		rec := s.receipts[id]
		if rec.seq == seq && rec.deadline.Equal(deadline) && (rec.origin == originPositive || rec.origin == originFresh) {
			return Acceptance{id: id, incarnation: s.incarnation, viewer: rec.viewer}, true
		}
	}
	return Acceptance{}, false
}

// Callers hold s.mu for writing.
func (s *Service) retireHashLocked(hash string) {
	for _, id := range s.idsByHash[hash] {
		delete(s.receipts, id)
	}
	delete(s.idsByHash, hash)
}

// retireCacheLocked drops personal-token receipts and keeps durable ones.
// Callers hold s.mu for writing.
func (s *Service) retireCacheLocked(hash string) {
	ids := s.idsByHash[hash]
	kept := ids[:0]
	for _, id := range ids {
		rec := s.receipts[id]
		if rec.origin == originDurable || rec.origin == originDelegation {
			kept = append(kept, id)
			continue
		}
		delete(s.receipts, id)
	}
	if len(kept) == 0 {
		delete(s.idsByHash, hash)
		return
	}
	s.idsByHash[hash] = kept
}

// Callers hold s.mu for writing.
func (s *Service) retireAllCacheLocked() {
	for hash := range s.idsByHash {
		s.retireCacheLocked(hash)
	}
}
