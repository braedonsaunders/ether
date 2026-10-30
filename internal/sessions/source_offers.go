package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

const sourceOfferNamespace = "sessions.source-offer"
const sourceOfferTTL = time.Hour
const maxSourceOffers = 8192
const maxSourceOfferID int64 = 1<<53 - 1

type sourceOffers struct {
	db       *sql.DB
	broker   *Broker
	protocol frontend.Protocol
	limits   capability.Limits
	maxTotal int
}
type offerSelector struct{ kind, reference, revision string }

func NewSourceOffers(db *sql.DB, b *Broker, protocol frontend.Protocol) (frontend.SourceOffers, error) {
	if db == nil || b == nil || protocol == "" {
		return nil, errors.New("sessions: missing source offer dependency")
	}
	return &sourceOffers{db: db, broker: b, protocol: protocol, limits: capability.Limits{PerScope: 256, PerShard: 4096}, maxTotal: maxSourceOffers}, nil
}

func (s *sourceOffers) authorize(ctx context.Context, scope frontend.VersionScope, item core.RatingKey) (frontend.VersionScope, error) {
	if scope.Protocol != s.protocol || scope.Device.ID == "" || len(scope.Device.ID) > 512 || item == 0 {
		return scope, core.ErrNotFound
	}
	viewer, err := s.broker.viewer(ctx, scope.Viewer)
	if err != nil {
		return scope, err
	}
	scope.Viewer = viewer
	scope.Caps.Protocol = s.protocol
	scope.Caps.ClientID = clientKey(s.protocol, scope.Device)
	if !s.broker.permitted(viewer, scope.Caps) {
		return scope, core.ErrUnauthorized
	}
	lookup := s.broker.deps.MediaLookup
	if lookup == nil {
		lookup = s.broker.deps.Lookup
	}
	title, err := lookup(ctx, viewer, scope.Device, item)
	if err != nil || title == nil || title.Key != item || title.Type == core.TypeShow || title.Type == core.TypeSeason || (!title.Released.IsZero() && title.Released.After(s.broker.deps.Now())) {
		return scope, core.ErrNotFound
	}
	return scope, nil
}

func (s *sourceOffers) headerScope(scope frontend.VersionScope) capability.HeaderScope {
	return capability.HeaderScope{Namespace: sourceOfferNamespace, ViewerID: scope.Viewer.ID, Surface: string(s.protocol), DeviceID: scope.Device.ID}
}

func (s *sourceOffers) selector(ctx context.Context, tx *sql.Tx, scope frontend.VersionScope, target frontend.SourceOfferTarget, allowed map[string]bool) (offerSelector, int64, error) {
	until := s.broker.deps.Now().Add(sourceOfferTTL).UnixMilli()
	now := s.broker.deps.Now().UnixMilli()
	if (target.VersionHandle == "") == (target.DetailSource == "") {
		return offerSelector{}, 0, core.ErrNotFound
	}
	kind, reference, revision := "version", normalizeVersionHandle(target.VersionHandle), ""
	var item, expires int64
	if target.DetailSource != "" {
		kind, reference, revision = "detail", normalizeVersionHandle(target.DetailSource), target.ETag
		if reference == "" || revision == "" || len(revision) > 512 {
			return offerSelector{}, 0, core.ErrNotFound
		}
		query := `SELECT item_id,expires FROM sessions_detail_sources WHERE handle=? AND viewer_id=? AND surface=? AND device_id=? AND revision=? AND expires>?`
		if store.DialectOf(s.db) == store.Postgres {
			query += ` FOR SHARE`
		}
		err := tx.QueryRowContext(ctx, query, reference, scope.Viewer.ID, string(s.protocol), scope.Device.ID, revision, now).Scan(&item, &expires)
		if err != nil {
			return offerSelector{}, 0, offerLookupError(err)
		}
	} else {
		if reference == "" || target.ETag != "" {
			return offerSelector{}, 0, core.ErrNotFound
		}
		digest := sha256.Sum256([]byte(reference))
		var release string
		err := tx.QueryRowContext(ctx, `SELECT v.item_id,v.release_id,h.expires FROM sessions_version_handles v JOIN capability_headers h ON h.namespace='sessions.version' AND h.digest=? AND h.viewer_id=v.viewer_id AND h.surface=v.surface AND h.device_id=v.device_id WHERE v.handle=? AND v.viewer_id=? AND v.surface=? AND v.device_id=? AND h.expires>?`, hex.EncodeToString(digest[:]), reference, scope.Viewer.ID, string(s.protocol), scope.Device.ID, now).Scan(&item, &release, &expires)
		if err != nil {
			return offerSelector{}, 0, offerLookupError(err)
		}
		if !allowed[release] {
			return offerSelector{}, 0, core.ErrNotFound
		}
	}
	if core.RatingKey(item) != target.Item {
		return offerSelector{}, 0, core.ErrNotFound
	}
	return offerSelector{kind, reference, revision}, min(until, expires), nil
}

func offerLookupError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return frontend.ErrSourceGone
	}
	return err
}

func (s *sourceOffers) Issue(ctx context.Context, scope frontend.VersionScope, target frontend.SourceOfferTarget) (frontend.SourceOffer, error) {
	offers, err := s.IssueBatch(ctx, scope, []frontend.SourceOfferTarget{target})
	if err != nil {
		return frontend.SourceOffer{}, err
	}
	return offers[0], nil
}

func (s *sourceOffers) IssueBatch(ctx context.Context, scope frontend.VersionScope, targets []frontend.SourceOfferTarget) ([]frontend.SourceOffer, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if len(targets) == 0 || len(targets) > 128 {
		return nil, core.ErrNotAvailable
	}
	item := targets[0].Item
	for _, target := range targets {
		if target.Item != item {
			return nil, core.ErrNotFound
		}
	}
	scope, err := s.authorize(ctx, scope, item)
	if err != nil {
		return nil, err
	}
	var allowed map[string]bool
	for _, target := range targets {
		if target.VersionHandle != "" {
			handles := &versionHandles{db: s.db, broker: s.broker, protocol: s.protocol}
			_, allowed, err = handles.allowedOffers(ctx, scope, item)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.db), tx, "sessions:source-offer-admission"); err != nil {
		return nil, err
	}
	hs := s.headerScope(scope)
	if err = capability.LockHeaderScopeInTx(ctx, s.db, tx, hs); err != nil {
		return nil, err
	}
	now := s.broker.deps.Now().UnixMilli()
	for n := 0; n < 4; n++ {
		if _, err = capability.PruneScopedHeadersInTx(ctx, tx, sourceOfferNamespace, scope.Viewer.ID, string(s.protocol), now, capability.HeaderBatchMax); err != nil {
			return nil, err
		}
		if _, err = capability.PruneHeadersInTx(ctx, tx, sourceOfferNamespace, now, capability.HeaderBatchMax); err != nil {
			return nil, err
		}
	}
	result := make([]frontend.SourceOffer, 0, len(targets))
	for _, target := range targets {
		selector, expires, err := s.selector(ctx, tx, scope, target, allowed)
		if err != nil {
			return nil, err
		}
		var id int64
		var digest string
		err = tx.QueryRowContext(ctx, `SELECT o.id,h.digest FROM sessions_source_offers o JOIN capability_headers h ON h.namespace=o.namespace AND h.digest=o.digest WHERE o.item_id=? AND o.kind=? AND o.reference=? AND o.revision=? AND h.viewer_id=? AND h.surface=? AND h.device_id=? AND h.expires>?`, int64(item), selector.kind, selector.reference, selector.revision, scope.Viewer.ID, string(s.protocol), scope.Device.ID, now).Scan(&id, &digest)
		if err == nil {
			renewed, renewErr := capability.RenewHeaderInTx(ctx, s.db, tx, capability.Header{HeaderScope: hs, Digest: digest, ExpiresMillis: expires}, now)
			if renewErr != nil {
				return nil, renewErr
			}
			if !renewed {
				return nil, frontend.ErrSourceGone
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=?`, sourceOfferNamespace).Scan(&count); err != nil {
				return nil, err
			}
			if count >= s.maxTotal {
				return nil, &capability.CapacityError{Scope: "source-offers"}
			}
			token, tokenErr := randomUUID()
			if tokenErr != nil {
				return nil, tokenErr
			}
			sum := sha256.Sum256([]byte(token))
			digest = hex.EncodeToString(sum[:])
			if err = capability.AdmitHeadersInTx(ctx, s.db, tx, []capability.Header{{HeaderScope: hs, Digest: digest, ExpiresMillis: expires}}, s.limits); err != nil {
				return nil, err
			}
			err = tx.QueryRowContext(ctx, `INSERT INTO sessions_source_offers(namespace,digest,item_id,kind,reference,revision) VALUES(?,?,?,?,?,?) RETURNING id`, sourceOfferNamespace, digest, int64(item), selector.kind, selector.reference, selector.revision).Scan(&id)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
		if id <= 1<<31-1 || id > maxSourceOfferID {
			return nil, core.ErrNotAvailable
		}
		resolved := frontend.SourceOfferTarget{Item: item}
		if selector.kind == "detail" {
			resolved.DetailSource, resolved.ETag = selector.reference, selector.revision
		} else {
			resolved.VersionHandle = selector.reference
		}
		result = append(result, frontend.SourceOffer{ID: strconv.FormatInt(id, 10), ExpiresAt: time.UnixMilli(expires).UTC(), Target: resolved})
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *sourceOffers) read(ctx context.Context, scope frontend.VersionScope, id string) (frontend.SourceOffer, capability.Header, error) {
	var out frontend.SourceOffer
	var h capability.Header
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 1<<31-1 || n > maxSourceOfferID || strconv.FormatInt(n, 10) != id {
		return out, h, frontend.ErrSourceGone
	}
	if scope.Protocol != s.protocol || scope.Viewer == nil || scope.Device.ID == "" {
		return out, h, core.ErrNotFound
	}
	var item, expires int64
	var kind, reference, revision string
	h.HeaderScope = s.headerScope(scope)
	err = s.db.QueryRowContext(ctx, `SELECT o.item_id,o.kind,o.reference,o.revision,h.digest,h.expires FROM sessions_source_offers o JOIN capability_headers h ON h.namespace=o.namespace AND h.digest=o.digest WHERE o.id=? AND h.viewer_id=? AND h.surface=? AND h.device_id=? AND h.expires>?`, n, scope.Viewer.ID, string(s.protocol), scope.Device.ID, s.broker.deps.Now().UnixMilli()).Scan(&item, &kind, &reference, &revision, &h.Digest, &expires)
	if err != nil {
		return out, h, offerLookupError(err)
	}
	out.ID, out.ExpiresAt, out.Target.Item = id, time.UnixMilli(expires).UTC(), core.RatingKey(item)
	h.ExpiresMillis = expires
	if kind == "detail" {
		out.Target.DetailSource, out.Target.ETag = reference, revision
	} else if kind == "version" {
		out.Target.VersionHandle = reference
	} else {
		return frontend.SourceOffer{}, h, frontend.ErrSourceGone
	}
	return out, h, nil
}

func (s *sourceOffers) Resolve(ctx context.Context, scope frontend.VersionScope, id string) (frontend.SourceOffer, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	viewer, err := s.broker.viewer(ctx, scope.Viewer)
	if err != nil {
		return frontend.SourceOffer{}, err
	}
	scope.Viewer = viewer
	out, _, err := s.read(ctx, scope, id)
	if err != nil {
		return out, err
	}
	scope, err = s.authorize(ctx, scope, out.Target.Item)
	if err != nil {
		return frontend.SourceOffer{}, err
	}
	var allowed map[string]bool
	if out.Target.VersionHandle != "" {
		h := &versionHandles{db: s.db, broker: s.broker, protocol: s.protocol}
		_, allowed, err = h.allowedOffers(ctx, scope, out.Target.Item)
		if err != nil {
			return frontend.SourceOffer{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return frontend.SourceOffer{}, err
	}
	defer tx.Rollback()
	if _, _, err = s.selector(ctx, tx, scope, out.Target, allowed); err != nil {
		return frontend.SourceOffer{}, err
	}
	// Recheck header revocation after policy/selector lookup before publishing.
	var live int
	number, _ := strconv.ParseInt(id, 10, 64)
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM capability_headers WHERE namespace=? AND digest=(SELECT digest FROM sessions_source_offers WHERE id=?) AND viewer_id=? AND surface=? AND device_id=? AND expires>?`, sourceOfferNamespace, number, scope.Viewer.ID, string(s.protocol), scope.Device.ID, s.broker.deps.Now().UnixMilli()).Scan(&live)
	if err != nil {
		return frontend.SourceOffer{}, offerLookupError(err)
	}
	if err = tx.Commit(); err != nil {
		return frontend.SourceOffer{}, err
	}
	return out, nil
}

func (s *sourceOffers) Revoke(ctx context.Context, scope frontend.VersionScope, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	viewer, err := s.broker.viewer(ctx, scope.Viewer)
	if err != nil {
		return err
	}
	scope.Viewer = viewer
	_, header, err := s.read(ctx, scope, id)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	removed, err := capability.ConsumeHeaderInTx(ctx, s.db, tx, header, s.broker.deps.Now().UnixMilli())
	if err != nil {
		return err
	}
	if !removed {
		return frontend.ErrSourceGone
	}
	return tx.Commit()
}
