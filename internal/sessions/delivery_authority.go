package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
)

const deliveryNamespace = "sessions.delivery"

type deliveryAuthority struct {
	ViewerID   int64
	ItemID     int64
	Surface    string
	DeviceID   string
	SessionID  string
	Generation int64
	ReleaseID  string
	Revision   string
	Token      int64
	Digest     string
}

func reserveDeliveryToken(ctx context.Context, db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	var token int64
	err := db.QueryRowContext(ctx, `INSERT INTO sessions_delivery_tokens DEFAULT VALUES RETURNING id`).Scan(&token)
	if err != nil || token <= 0 {
		if err == nil {
			err = errors.New("sessions: delivery token was not issued")
		}
		return 0, err
	}
	return token, nil
}

func (b *Broker) bindDeliveryAuthority(ctx context.Context, s *brokerSession, ps *core.PlaySession) error {
	if b == nil || b.deps.DB == nil || s == nil {
		return nil
	}
	b.mu.Lock()
	live := b.sessions[s.public.ID]
	if live == nil || live.playID != s.playID {
		b.mu.Unlock()
		return core.ErrNotAvailable
	}
	token := live.deliveryToken
	b.mu.Unlock()
	if token <= 0 {
		var err error
		token, err = reserveDeliveryToken(ctx, b.deps.DB)
		if err != nil {
			return err
		}
		b.mu.Lock()
		live = b.sessions[s.public.ID]
		if live == nil || live.playID != s.playID {
			b.mu.Unlock()
			return core.ErrNotAvailable
		}
		if live.deliveryToken <= 0 {
			live.deliveryToken = token
		} else {
			token = live.deliveryToken
		}
		b.mu.Unlock()
	}
	b.mu.Lock()
	live = b.sessions[s.public.ID]
	if live == nil || live.playID != s.playID || live.deliveryToken != token {
		b.mu.Unlock()
		return core.ErrNotAvailable
	}
	live.deliveryGeneration++
	generation := live.deliveryGeneration
	sessionID := live.deliverySession
	b.mu.Unlock()
	release := s.playID
	if ps != nil && ps.Resolution != nil && ps.Resolution.Release.ID != "" {
		release = ps.Resolution.Release.ID
	}
	revision := sourceRevision(ps)
	if revision == "" {
		revision = release
	}
	// A lost compare-and-swap leaves the newer authority in place. The stream
	// still starts; an older token cannot replace that row.
	_, err := publishDeliveryAuthority(ctx, b.deps.DB, deliveryAuthority{
		ViewerID: s.public.ViewerID, ItemID: int64(s.public.Item), Surface: string(s.public.Protocol),
		DeviceID: s.public.Device.ID, SessionID: sessionID, Generation: generation, ReleaseID: release,
		Revision: revision, Token: token,
	}, b.deps.Now())
	return err
}

func (b *Broker) rememberedDeliveryRelease(ctx context.Context, viewer int64, surface, device string, item core.RatingKey, sessionID string) (string, error) {
	if b == nil || b.deps.DB == nil || sessionID == "" {
		return "", nil
	}
	row, ok, err := lookupDeliveryAuthority(ctx, b.deps.DB, viewer, int64(item), surface, device)
	if err != nil || !ok || row.SessionID != sessionID || row.ReleaseID == "" {
		return "", err
	}
	if err = renewDeliveryAuthority(ctx, b.deps.DB, row, b.deps.Now()); err != nil {
		// The header is the grant. Once it has expired or been removed, this
		// row is no longer authority. Silo reuses one installation id as the
		// client session, so failing the plan here would block every later
		// play of the title on that device. A new plan publishes a new grant.
		if errors.Is(err, core.ErrNotAvailable) {
			return "", nil
		}
		return "", err
	}
	return row.ReleaseID, nil
}

func publishDeliveryAuthority(ctx context.Context, db *sql.DB, next deliveryAuthority, now time.Time) (bool, error) {
	if db == nil || next.ViewerID <= 0 || next.ItemID == 0 || next.Surface == "" || next.SessionID == "" || next.ReleaseID == "" || next.Revision == "" || next.Generation <= 0 || next.Token <= 0 || now.IsZero() {
		return false, errors.New("sessions: invalid delivery authority")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	scope := capability.HeaderScope{Namespace: deliveryNamespace, ViewerID: next.ViewerID, Surface: next.Surface, DeviceID: next.DeviceID}
	if err = capability.LockHeaderScopeInTx(ctx, db, tx, scope); err != nil {
		return false, err
	}
	var issued int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions_delivery_tokens WHERE id=?`, next.Token).Scan(&issued); err != nil {
		return false, err
	}
	var current deliveryAuthority
	err = tx.QueryRowContext(ctx, `SELECT session_id,generation,release_id,source_revision,token,header_digest FROM sessions_delivery_authority WHERE viewer_id=? AND item_id=? AND surface=? AND device_id=?`, next.ViewerID, next.ItemID, next.Surface, next.DeviceID).Scan(&current.SessionID, &current.Generation, &current.ReleaseID, &current.Revision, &current.Token, &current.Digest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	replace := errors.Is(err, sql.ErrNoRows) || next.Token > current.Token || (next.Token == current.Token && next.SessionID == current.SessionID && next.Generation > current.Generation)
	if !replace {
		return false, nil
	}
	digest := lazyHash(fmt.Sprintf("%d\x00%s\x00%d\x00%s\x00%s", next.Token, next.SessionID, next.Generation, next.ReleaseID, next.Revision))
	header := capability.Header{HeaderScope: scope, Digest: digest, ExpiresMillis: now.Add(2 * time.Hour).UnixMilli()}
	if current.Digest != "" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM capability_claims WHERE namespace=? AND digest=? AND purpose=?`, deliveryNamespace, current.Digest, capability.RetirementOwnerClaim); err != nil {
			return false, err
		}
		if err = capability.DropHeadersInTx(ctx, tx, scope, []string{current.Digest}); err != nil {
			return false, err
		}
	}
	if err = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{header}, capability.Limits{PerScope: 256, PerShard: 4096}); err != nil {
		return false, err
	}
	claimed, err := capability.ClaimHeaderInTx(ctx, db, tx, header, capability.RetirementOwnerClaim, now.UnixMilli())
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, core.ErrNotAvailable
	}
	if current.Digest == "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions_delivery_authority(viewer_id,item_id,surface,device_id,session_id,generation,release_id,source_revision,token,header_digest) VALUES(?,?,?,?,?,?,?,?,?,?)`, next.ViewerID, next.ItemID, next.Surface, next.DeviceID, next.SessionID, next.Generation, next.ReleaseID, next.Revision, next.Token, digest)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE sessions_delivery_authority SET session_id=?,generation=?,release_id=?,source_revision=?,token=?,header_digest=? WHERE viewer_id=? AND item_id=? AND surface=? AND device_id=? AND token=? AND generation=?`, next.SessionID, next.Generation, next.ReleaseID, next.Revision, next.Token, digest, next.ViewerID, next.ItemID, next.Surface, next.DeviceID, current.Token, current.Generation)
		if err == nil {
			var n int64
			n, err = result.RowsAffected()
			if err == nil && n != 1 {
				return false, nil
			}
		}
	}
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func lookupDeliveryAuthority(ctx context.Context, db *sql.DB, viewer, item int64, surface, device string) (deliveryAuthority, bool, error) {
	var row deliveryAuthority
	err := db.QueryRowContext(ctx, `SELECT viewer_id,item_id,surface,device_id,session_id,generation,release_id,source_revision,token,header_digest FROM sessions_delivery_authority WHERE viewer_id=? AND item_id=? AND surface=? AND device_id=?`, viewer, item, surface, device).Scan(&row.ViewerID, &row.ItemID, &row.Surface, &row.DeviceID, &row.SessionID, &row.Generation, &row.ReleaseID, &row.Revision, &row.Token, &row.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return deliveryAuthority{}, false, nil
	}
	if err != nil {
		return deliveryAuthority{}, false, err
	}
	return row, true, nil
}

func renewDeliveryAuthority(ctx context.Context, db *sql.DB, row deliveryAuthority, now time.Time) error {
	if db == nil || row.Digest == "" || now.IsZero() {
		return errors.New("sessions: invalid delivery renewal")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	header := capability.Header{HeaderScope: capability.HeaderScope{Namespace: deliveryNamespace, ViewerID: row.ViewerID, Surface: row.Surface, DeviceID: row.DeviceID}, Digest: row.Digest, ExpiresMillis: now.Add(2 * time.Hour).UnixMilli()}
	ok, err := capability.RenewHeaderInTx(ctx, db, tx, header, now.UnixMilli())
	if err != nil || !ok {
		if err == nil {
			err = core.ErrNotAvailable
		}
		return err
	}
	return tx.Commit()
}
