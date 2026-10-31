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
	imagePayloadCleanup   = 300
	imageHeaderCleanup    = 300
	imageMaintenanceLimit = 1200
)

type imageAdmission struct {
	perScope, perShard, payloadCleanup, headerCleanup int
}

func defaultImageAdmission() imageAdmission {
	// Fixed domain quotas apply to every issuance, including empty devices.
	return imageAdmission{perScope: imagePerScope, perShard: imagePerShard, payloadCleanup: imagePayloadCleanup, headerCleanup: imageHeaderCleanup}
}

// ImageCapacityError means this viewer's or shard's image-header admission is full.
// Every image payload must have a charged reservation.
type ImageCapacityError struct{ Scope string }

func (e *ImageCapacityError) Error() string {
	return "identity: image grant capacity reached (" + e.Scope + ")"
}

func imageShardKey(viewer int64) string {
	return fmt.Sprintf("capability:admission:image:%d", viewer%capability.HeaderShardCount)
}

// insertAdmitted authorizes and admits images within the caller transaction.
func (s *ImageGrantService) insertAdmitted(ctx context.Context, tx *sql.Tx, viewerID int64, source, deviceID, deviceJSON, channel string, rows []imageBatchRow) error {
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
	cleanup := func() error {
		if s.beforeImageCleanup != nil {
			s.beforeImageCleanup(tx)
		}
		if err := s.deleteExpiredImages(ctx, tx, viewerID, now); err != nil {
			return err
		}
		return s.deleteExpiredImageHeaders(ctx, tx, viewerID, now)
	}
	// A page issues hundreds of images, each in its own write transaction;
	// pruning this viewer's expired rows in every one of them made the
	// prune the hottest write on the server. Prune at most every
	// imageCleanupEvery per viewer, and whenever admission runs out of room,
	// which is when the pruned rows matter.
	if s.imageCleanupDue(viewerID) {
		if err := cleanup(); err != nil {
			return err
		}
	}
	expires := now + s.ttl.Milliseconds()
	err := s.admitHeaders(ctx, tx, viewerID, string(s.surface), deviceID, expires, rows)
	var full *ImageCapacityError
	if errors.As(err, &full) {
		if err = cleanup(); err != nil {
			return err
		}
		err = s.admitHeaders(ctx, tx, viewerID, string(s.surface), deviceID, expires, rows)
	}
	if err != nil {
		return err
	}
	for start := 0; start < len(rows); start += imageBatchInsertChunk {
		end := min(start+imageBatchInsertChunk, len(rows))
		if err := insertImagePayloads(ctx, tx, source, viewerID, string(s.surface), deviceJSON, channel, expires, rows[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func insertImagePayloads(ctx context.Context, tx *sql.Tx, source string, viewerID int64, surface, deviceJSON, channel string, expires int64, rows []imageBatchRow) error {
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

func (s *ImageGrantService) admitHeaders(ctx context.Context, tx *sql.Tx, viewerID int64, surface, deviceID string, expires int64, rows []imageBatchRow) error {
	scope := capability.HeaderScope{Namespace: imageCapabilityNamespace, ViewerID: viewerID, Surface: surface, DeviceID: deviceID}
	for start := 0; start < len(rows); start += capability.HeaderBatchMax {
		end := min(start+capability.HeaderBatchMax, len(rows))
		headers := make([]capability.Header, 0, end-start)
		for _, row := range rows[start:end] {
			headers = append(headers, capability.Header{HeaderScope: scope, Digest: row.hash, ExpiresMillis: expires})
		}
		if err := capability.AdmitHeadersInTx(ctx, s.db, tx, headers, capability.Limits{PerScope: s.admission.perScope, PerShard: s.admission.perShard}); err != nil {
			var capacity *capability.CapacityError
			if errors.As(err, &capacity) {
				return &ImageCapacityError{Scope: capacity.Scope}
			}
			return err
		}
	}
	return nil
}

// imageCleanupEvery bounds how often one viewer's expired image rows are
// pruned on the issue path; PruneImageGrants sweeps the rest.
const imageCleanupEvery = 30 * time.Second

func (s *ImageGrantService) imageCleanupDue(viewerID int64) bool {
	now := time.Now()
	s.cleanMu.Lock()
	defer s.cleanMu.Unlock()
	if s.cleaned == nil {
		s.cleaned = map[int64]time.Time{}
	}
	if last, ok := s.cleaned[viewerID]; ok && now.Sub(last) < imageCleanupEvery {
		return false
	}
	if len(s.cleaned) > 4096 {
		clear(s.cleaned)
	}
	s.cleaned[viewerID] = now
	return true
}

func (s *ImageGrantService) deleteExpiredImages(ctx context.Context, tx *sql.Tx, viewerID, now int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM frontend_image_grants WHERE viewer_id=? AND surface=? AND expires<=? AND hash IN (SELECT hash FROM frontend_image_grants WHERE viewer_id=? AND surface=? AND expires<=? ORDER BY expires,hash LIMIT ?)`, viewerID, string(s.surface), now, viewerID, string(s.surface), now, s.admission.payloadCleanup)
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

// revokeAdmitted marks the legacy row revoked and drops the matching header.
// Unknown headers are left alone by DropHeadersInTx and are not an authorization signal.
// Lock order matches insert: shard advisory lock, then the legacy row, then the header drop.
func (s *ImageGrantService) revokeAdmitted(ctx context.Context, viewerID int64, source, digest string) error {
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
	{
		scope := capability.HeaderScope{Namespace: imageCapabilityNamespace, ViewerID: viewerID, Surface: string(s.surface), DeviceID: device.ID}
		if err = capability.DropHeadersInTx(ctx, tx, scope, []string{digest}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneImageGrants removes expired legacy image grants and expired image headers.
// Callers hold every image shard lock
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
		if err = store.LockKey(ctx, dialect, tx, fmt.Sprintf("capability:admission:image:%d", shard)); err != nil {
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

// PruneImageGrantShard performs one bounded maintenance transaction for one
// shard. Scheduling and rotation across shards belong to its caller. It takes
// only that shard's issue/revoke lock, and rechecks expiry and shard in each
// outer delete. The caller supplies the same trusted clock as image issuance.
// Issued scope and expiry are immutable after cutover.
func PruneImageGrantShard(ctx context.Context, db *sql.DB, now time.Time, shard int64) (int64, error) {
	if db == nil || now.IsZero() || shard < 0 || shard >= capability.HeaderShardCount {
		return 0, errors.New("identity: invalid image maintenance scope")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(db), tx, fmt.Sprintf("capability:admission:image:%d", shard)); err != nil {
		return 0, err
	}
	// The fixed expression matches migration 6. A future modulus change must
	// migrate this index and coordinate old writers before enabling cleanup.
	if capability.HeaderShardCount != 64 {
		return 0, errors.New("identity: image shard index modulus mismatch")
	}
	ms := now.UnixMilli()
	res, err := tx.ExecContext(ctx, `DELETE FROM frontend_image_grants WHERE expires<=? AND (viewer_id % 64)=? AND hash IN (SELECT hash FROM frontend_image_grants WHERE expires<=? AND (viewer_id % 64)=? ORDER BY expires,hash LIMIT ?)`, ms, shard, ms, shard, capability.HeaderBatchMax)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND shard=? AND expires<=? AND digest IN (SELECT digest FROM capability_headers WHERE namespace=? AND shard=? AND expires<=? ORDER BY expires,digest LIMIT ?)`, imageCapabilityNamespace, shard, ms, imageCapabilityNamespace, shard, ms, capability.HeaderBatchMax)
	if err != nil {
		return 0, err
	}
	m, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n + m, nil
}
