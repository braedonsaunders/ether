package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

const imageCapabilityNamespace = "image"

// A catalog page stores at most maxImageBatchItems (50) grants for each of six
// kinds, which is 300 rows. The production image TTL is 10 minutes, so a viewer
// can refresh several shelves before the oldest page expires. Eight such pages
// is the per-viewer admission. Four viewers can share one header shard.
// HeaderBatchMax (40) is only the shared statement size, not this quota.
const (
	imagePerScope         = 8 * 300
	imagePerShard         = 4 * imagePerScope
	imageLegacyCleanup    = 300
	imageHeaderCleanup    = 300
	imageMaintenanceLimit = 1200
)

type imageAdmission struct {
	perScope, perShard, legacyCleanup, headerCleanup int
}

func defaultImageAdmission() imageAdmission {
	return imageAdmission{imagePerScope, imagePerShard, imageLegacyCleanup, imageHeaderCleanup}
}

// ImageCapacityError means this viewer's or shard's image-header admission is full.
// Rows written only to frontend_image_grants by an older binary are not counted.
type ImageCapacityError struct{ Scope string }

func (e *ImageCapacityError) Error() string {
	return "identity: image grant capacity reached (" + e.Scope + ")"
}

func imageShardKey(viewer int64) string {
	return fmt.Sprintf("capability:image:%d", viewer%capability.HeaderShardCount)
}

// insertMirrored writes the legacy image grant, which remains the read and
// revocation authority, and the shared header mirror in the same transaction.
// tx must already be open on s.db. Any error requires the caller to roll back.
//
// Lock order, and no other capability lock:
//  1. store.LockKey capability:image:<viewerID%HeaderShardCount>
//  2. re-read the source grant and active user (FOR UPDATE on PostgreSQL)
//  3. delete expired legacy rows for this viewer and surface
//  4. PruneScopedHeadersInTx for this viewer and surface (no lock inside the helper)
//  5. count image headers only, then insert legacy rows, then BindHeadersInTx
func (s *ImageGrantService) insertMirrored(ctx context.Context, tx *sql.Tx, viewerID int64, source, deviceID, deviceJSON, channel string, rows []imageBatchRow) error {
	if tx == nil || viewerID <= 0 || len(rows) == 0 {
		return core.ErrUnauthorized
	}
	if err := store.LockKey(ctx, store.DialectOf(s.db), tx, imageShardKey(viewerID)); err != nil {
		return err
	}
	now := s.now().UnixMilli()
	check := `SELECT g.hash FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.viewer_id=? AND g.surface=? AND g.revoked=0 AND g.expires>? AND u.active=1`
	if store.DialectOf(s.db) == store.Postgres {
		check += ` FOR UPDATE OF g,u`
	}
	var verified string
	if err := tx.QueryRowContext(ctx, check, source, viewerID, string(s.surface), now).Scan(&verified); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrUnauthorized
		}
		return err
	}
	if s.beforeImageCleanup != nil {
		s.beforeImageCleanup(tx)
	}
	if err := s.deleteExpiredLegacy(ctx, tx, viewerID, now); err != nil {
		return err
	}
	if err := s.deleteExpiredImageHeaders(ctx, tx, viewerID, now); err != nil {
		return err
	}
	expires := now + s.ttl.Milliseconds()
	if deviceID != "" {
		if err := s.admitImageHeaders(ctx, tx, viewerID, len(rows)); err != nil {
			return err
		}
	}
	for start := 0; start < len(rows); start += imageBatchInsertChunk {
		end := min(start+imageBatchInsertChunk, len(rows))
		if err := insertLegacyImageGrants(ctx, tx, source, viewerID, string(s.surface), deviceJSON, channel, expires, rows[start:end]); err != nil {
			return err
		}
	}
	return bindImageHeaders(ctx, tx, viewerID, string(s.surface), deviceID, expires, rows)
}

func insertLegacyImageGrants(ctx context.Context, tx *sql.Tx, source string, viewerID int64, surface, deviceJSON, channel string, expires int64, rows []imageBatchRow) error {
	var query strings.Builder
	query.WriteString(`INSERT INTO frontend_image_grants(hash,source_hash,viewer_id,surface,item,device,image_kind,image_path,image_tag,expires,channel,image_subject) VALUES `)
	args := make([]any, 0, len(rows)*12)
	for i, row := range rows {
		if i > 0 {
			query.WriteByte(',')
		}
		query.WriteString(`(?,?,?,?,?,?,?,?,?,?,?,?)`)
		args = append(args, row.hash, source, viewerID, surface, int64(row.item), deviceJSON, row.image.Kind, row.image.Path, row.image.Tag, expires, channel, row.image.Subject)
	}
	result, err := tx.ExecContext(ctx, query.String(), args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != int64(len(rows)) {
		return core.ErrUnauthorized
	}
	return nil
}

// bindImageHeaders mirrors digests after the legacy insert. Empty device ids are
// accepted by HeaderScope, and this domain still leaves those grants legacy-only.
// They are not charged against the header quota. There is no header-only read path.
func bindImageHeaders(ctx context.Context, tx *sql.Tx, viewerID int64, surface, deviceID string, expires int64, rows []imageBatchRow) error {
	if deviceID == "" {
		return nil
	}
	scope := capability.HeaderScope{Namespace: imageCapabilityNamespace, ViewerID: viewerID, Surface: surface, DeviceID: deviceID}
	for start := 0; start < len(rows); start += capability.HeaderBatchMax {
		end := min(start+capability.HeaderBatchMax, len(rows))
		headers := make([]capability.Header, 0, end-start)
		for _, row := range rows[start:end] {
			headers = append(headers, capability.Header{HeaderScope: scope, Digest: row.hash, ExpiresMillis: expires})
		}
		if err := capability.BindHeadersInTx(ctx, tx, headers); err != nil {
			return err
		}
	}
	return nil
}

func (s *ImageGrantService) admitImageHeaders(ctx context.Context, tx *sql.Tx, viewerID int64, adding int) error {
	shard := viewerID % capability.HeaderShardCount
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND shard=? AND viewer_id=? AND surface=?`, imageCapabilityNamespace, shard, viewerID, string(s.surface)).Scan(&count); err != nil {
		return err
	}
	if count+adding > s.admission.perScope {
		return &ImageCapacityError{Scope: "viewer"}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND shard=?`, imageCapabilityNamespace, shard).Scan(&count); err != nil {
		return err
	}
	if count+adding > s.admission.perShard {
		return &ImageCapacityError{Scope: "shard"}
	}
	return nil
}

func (s *ImageGrantService) deleteExpiredLegacy(ctx context.Context, tx *sql.Tx, viewerID, now int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM frontend_image_grants WHERE viewer_id=? AND surface=? AND expires<=? AND hash IN (SELECT hash FROM frontend_image_grants WHERE viewer_id=? AND surface=? AND expires<=? ORDER BY expires,hash LIMIT ?)`, viewerID, string(s.surface), now, viewerID, string(s.surface), now, s.admission.legacyCleanup)
	return err
}

func (s *ImageGrantService) deleteExpiredImageHeaders(ctx context.Context, tx *sql.Tx, viewerID, now int64) error {
	left := s.admission.headerCleanup
	for left > 0 {
		n := left
		if n > capability.HeaderBatchMax {
			n = capability.HeaderBatchMax
		}
		removed, err := capability.PruneScopedHeadersInTx(ctx, tx, imageCapabilityNamespace, viewerID, string(s.surface), now, n)
		if err != nil {
			return err
		}
		left -= int(removed)
		if removed < int64(n) {
			return nil
		}
	}
	return nil
}

// revokeMirrored marks the legacy row revoked and drops the matching header.
// Unknown headers are left alone by DropHeadersInTx and are not an authorization signal.
// Lock order matches insert: shard advisory lock, then the legacy row, then the header drop.
func (s *ImageGrantService) revokeMirrored(ctx context.Context, viewerID int64, source, digest string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.db), tx, imageShardKey(viewerID)); err != nil {
		return err
	}
	var deviceJSON string
	err = tx.QueryRowContext(ctx, `UPDATE frontend_image_grants SET revoked=1 WHERE hash=? AND source_hash=? AND viewer_id=? AND surface=? RETURNING device`, digest, source, viewerID, string(s.surface)).Scan(&deviceJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrUnauthorized
	}
	if err != nil {
		return err
	}
	var device core.Device
	if json.Unmarshal([]byte(deviceJSON), &device) != nil {
		return core.ErrUnauthorized
	}
	if device.ID != "" {
		scope := capability.HeaderScope{Namespace: imageCapabilityNamespace, ViewerID: viewerID, Surface: string(s.surface), DeviceID: device.ID}
		if err = capability.DropHeadersInTx(ctx, tx, scope, []string{digest}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneImageGrants removes expired legacy image grants and expired image headers.
// It is not wired into process housekeeping. Callers hold every image shard lock
// in ascending order before either delete, and never take those locks in another order.
// Header deletion uses PruneHeadersInTx, which is namespace-global and capped at 40 rows.
func PruneImageGrants(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	if db == nil || now.IsZero() {
		return 0, errors.New("identity: image grant maintenance requires a database and clock")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	dialect := store.DialectOf(db)
	for shard := int64(0); shard < capability.HeaderShardCount; shard++ {
		if err = store.LockKey(ctx, dialect, tx, fmt.Sprintf("capability:image:%d", shard)); err != nil {
			return 0, err
		}
	}
	ms := now.UnixMilli()
	var removed int64
	left := imageMaintenanceLimit
	for left > 0 {
		n := left
		if n > capability.HeaderBatchMax {
			n = capability.HeaderBatchMax
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM frontend_image_grants WHERE expires<=? AND hash IN (SELECT hash FROM frontend_image_grants WHERE expires<=? ORDER BY expires,hash LIMIT ?)`, ms, ms, n)
		if err != nil {
			return 0, err
		}
		n64, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		removed += n64
		left -= int(n64)
		if n64 < int64(n) {
			break
		}
	}
	left = imageMaintenanceLimit
	for left > 0 {
		n := left
		if n > capability.HeaderBatchMax {
			n = capability.HeaderBatchMax
		}
		n64, err := capability.PruneHeadersInTx(ctx, tx, imageCapabilityNamespace, ms, n)
		if err != nil {
			return 0, err
		}
		removed += n64
		left -= int(n64)
		if n64 < int64(n) {
			break
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}
