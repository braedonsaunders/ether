package identity

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

// authorizePersonGrant is Silo-only. Descriptor I/O happens before its callback
// takes joint DB locks. The host holds its local config lease until the callback
// has committed and its own final epoch/config proof has also succeeded.
// No output may be published on either callback or host validation failure.
func (s *ImageGrantService) authorizePersonGrant(ctx context.Context, viewer *core.User, device core.Device, item core.RatingKey, channel string, image frontend.Image, hash, sourceHash, deviceJSON string) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	people, ok := s.experience().(frontend.PeopleImageReader)
	if !ok {
		return core.ErrNotAvailable
	}
	started, grantStarted := time.Now(), s.now()
	epoch, err := viewerpolicy.Read(ctx, s.db)
	if err != nil {
		return err
	}
	return people.AuthorizePersonImage(ctx, viewer, device, item, image, func(ctx context.Context) error {
		checked := s.now()
		if checked.UTC().Before(grantStarted.UTC()) {
			return core.ErrUnauthorized
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := viewerpolicy.Fence(ctx, s.db, tx, epoch); err != nil {
			return err
		}
		query := `SELECT i.expires,g.expires FROM frontend_image_grants i JOIN capability_headers h ON h.namespace='image' AND h.digest=i.hash AND h.viewer_id=i.viewer_id AND h.surface=i.surface AND h.expires=i.expires JOIN identity_grants g ON g.hash=i.source_hash AND g.viewer_id=i.viewer_id AND g.surface=i.surface JOIN auth_users u ON u.id=i.viewer_id WHERE i.hash=? AND i.source_hash=? AND i.surface='silo' AND i.viewer_id=? AND i.item=? AND i.device=? AND h.device_id=? AND g.device_id=? AND i.channel=? AND i.image_kind=? AND i.image_path=? AND i.image_tag=? AND i.image_subject=? AND i.revoked=0 AND i.expires>? AND g.revoked=0 AND g.expires>? AND u.active=1`
		if store.DialectOf(s.db) == store.Postgres {
			query += ` FOR SHARE OF i,h,g`
		}
		var imageExpiry, sourceExpiry int64
		if err := tx.QueryRowContext(ctx, query, hash, sourceHash, viewer.ID, int64(item), deviceJSON, device.ID, device.ID, channel, image.Kind, image.Path, image.Tag, image.Subject, checked.UnixMilli(), checked.UnixMilli()).Scan(&imageExpiry, &sourceExpiry); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		finished, elapsed := s.now(), time.Since(started)
		if finished.UTC().Before(checked.UTC()) || elapsed < 0 || elapsed > 4*time.Second || finished.UnixMilli() >= imageExpiry || finished.UnixMilli() >= sourceExpiry {
			return core.ErrUnauthorized
		}
		return nil
	})
}
