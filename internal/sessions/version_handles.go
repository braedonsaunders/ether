package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles/capability"
)

type versionHandles struct {
	db       *sql.DB
	broker   *Broker
	protocol frontend.Protocol
	limits   capability.Limits
}

// NewVersionHandles creates a surface-bound, durable version identifier map.
// Handles are not credentials and all lookups recheck viewer and title policy.
func NewVersionHandles(db *sql.DB, b *Broker, protocol frontend.Protocol) (frontend.VersionHandles, error) {
	if db == nil || b == nil || protocol == "" {
		return nil, errors.New("sessions: missing version handle dependency")
	}
	return &versionHandles{db: db, broker: b, protocol: protocol, limits: capability.Limits{PerScope: 100000, PerShard: 400000}}, nil
}

func (v *versionHandles) validate(ctx context.Context, scope frontend.VersionScope, item core.RatingKey, release string) (*core.Viewer, error) {
	viewer, offered, err := v.allowedOffers(ctx, scope, item)
	if err != nil {
		return nil, err
	}
	if release == "" || len(release) > 512 || !offered[release] {
		return nil, core.ErrNotFound
	}
	return viewer, nil
}

func (v *versionHandles) allowedOffers(ctx context.Context, scope frontend.VersionScope, item core.RatingKey) (*core.Viewer, map[string]bool, error) {
	if scope.Protocol != v.protocol || scope.Device.ID == "" || len(scope.Device.ID) > 512 || item == 0 || v.broker.deps.Variants == nil {
		return nil, nil, core.ErrNotFound
	}
	viewer, err := v.broker.viewer(ctx, scope.Viewer)
	if err != nil {
		return nil, nil, err
	}
	caps := scope.Caps
	caps.Protocol = v.protocol
	caps.ClientID = clientKey(v.protocol, scope.Device)
	viewer, err = v.broker.confirm(ctx, viewer, caps)
	if err != nil {
		return nil, nil, err
	}
	if caps.Remote && v.broker.deps.RemoteBitrateLimit != nil {
		if limit := v.broker.deps.RemoteBitrateLimit(); limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
			caps.MaxBitrateKbps = limit
		}
	}
	if limit := v.broker.deps.BitrateLimit(viewer); limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
		caps.MaxBitrateKbps = limit
	}
	title, err := v.broker.deps.Lookup(ctx, viewer, scope.Device, item)
	if err != nil || title == nil || title.Key != item || (!title.Released.IsZero() && title.Released.After(v.broker.deps.Now())) {
		return nil, nil, core.ErrNotFound
	}
	if title.Type == core.TypeShow || title.Type == core.TypeSeason {
		return nil, nil, core.ErrNotFound
	}
	offered, err := v.broker.deps.Variants(ctx, viewer, title, caps)
	if err != nil {
		return nil, nil, err
	}
	allowed := make(map[string]bool, len(offered))
	for _, candidate := range offered {
		allowed[candidate.ID] = true
	}
	return viewer, allowed, nil
}

func (v *versionHandles) IssueBatch(ctx context.Context, scope frontend.VersionScope, item core.RatingKey, releases []string) (map[string]string, error) {
	if len(releases) == 0 || len(releases) > 128 {
		return nil, core.ErrNotAvailable
	}
	viewer, offered, err := v.allowedOffers(ctx, scope, item)
	if err != nil {
		return nil, err
	}
	for _, release := range releases {
		if release == "" || len(release) > 512 || !offered[release] {
			return nil, core.ErrNotFound
		}
	}
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make(map[string]string, len(releases))
	ordered := slices.Clone(releases)
	slices.Sort(ordered)
	for _, release := range ordered {
		if _, exists := out[release]; exists {
			continue
		}
		token, err := randomUUID()
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions_version_handles(handle,viewer_id,surface,device_id,item_id,release_id) VALUES(?,?,?,?,?,?) ON CONFLICT(viewer_id,surface,device_id,item_id,release_id) DO NOTHING`, token, viewer.ID, string(v.protocol), scope.Device.ID, int64(item), release)
		if err != nil {
			return nil, err
		}
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT handle FROM sessions_version_handles WHERE viewer_id=? AND surface=? AND device_id=? AND item_id=? AND release_id=?`, viewer.ID, string(v.protocol), scope.Device.ID, int64(item), release).Scan(&stored); err != nil {
			return nil, err
		}
		out[release] = stored
	}
	headers := make([]capability.Header, 0, len(out))
	for _, handle := range out {
		digest := sha256.Sum256([]byte(handle))
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM capability_headers WHERE namespace=? AND digest=? AND viewer_id=? AND surface=? AND device_id=?`, "sessions.version", hex.EncodeToString(digest[:]), viewer.ID, string(v.protocol), scope.Device.ID).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		headers = append(headers, capability.Header{HeaderScope: capability.HeaderScope{Namespace: "sessions.version", ViewerID: viewer.ID, Surface: string(v.protocol), DeviceID: scope.Device.ID}, Digest: hex.EncodeToString(digest[:]), ExpiresMillis: math.MaxInt64})
	}
	for start := 0; start < len(headers); start += capability.HeaderBatchMax {
		if err := capability.AdmitHeadersInTx(ctx, v.db, tx, headers[start:min(start+capability.HeaderBatchMax, len(headers))], v.limits); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *versionHandles) Issue(ctx context.Context, scope frontend.VersionScope, item core.RatingKey, release string) (string, error) {
	handles, err := v.IssueBatch(ctx, scope, item, []string{release})
	if err != nil {
		return "", err
	}
	return handles[release], nil
}

func (v *versionHandles) Resolve(ctx context.Context, scope frontend.VersionScope, handle string) (frontend.VersionTarget, error) {
	item, release, err := v.resolveRecord(ctx, scope, handle)
	return frontend.VersionTarget{Item: item, Release: release}, err
}

func (v *versionHandles) resolveRecord(ctx context.Context, scope frontend.VersionScope, handle string) (core.RatingKey, string, error) {
	handle = normalizeVersionHandle(handle)
	if scope.Protocol != v.protocol || scope.Device.ID == "" || scope.Viewer == nil || handle == "" {
		return 0, "", core.ErrNotFound
	}
	viewer, err := v.broker.viewer(ctx, scope.Viewer)
	if err != nil {
		return 0, "", err
	}
	var item int64
	var release string
	digest := sha256.Sum256([]byte(handle))
	err = v.db.QueryRowContext(ctx, `SELECT v.item_id,v.release_id FROM sessions_version_handles v JOIN capability_headers h ON h.namespace='sessions.version' AND h.digest=? AND h.viewer_id=v.viewer_id AND h.surface=v.surface AND h.device_id=v.device_id WHERE v.handle=? AND v.viewer_id=? AND v.surface=? AND v.device_id=? AND h.expires>?`, hex.EncodeToString(digest[:]), handle, viewer.ID, string(v.protocol), scope.Device.ID, v.broker.deps.Now().UnixMilli()).Scan(&item, &release)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", core.ErrNotFound
	}
	if err != nil {
		return 0, "", err
	}
	if _, err = v.validate(ctx, scope, core.RatingKey(item), release); err != nil {
		return 0, "", err
	}
	return core.RatingKey(item), release, nil
}

func (v *versionHandles) MatchSession(ctx context.Context, scope frontend.VersionScope, handle, sessionID string) error {
	handle = normalizeVersionHandle(handle)
	item, release, err := v.resolveRecord(ctx, scope, handle)
	if err != nil {
		return err
	}
	s, _, err := v.broker.access(ctx, frontend.SessionAccess{Viewer: scope.Viewer, Device: scope.Device, Protocol: scope.Protocol}, sessionID)
	if err != nil {
		return err
	}
	if s.public.Item != item || s.versionHandle != handle || s.exactRelease != release {
		return core.ErrNotFound
	}
	ps, ok := v.broker.deps.Playback.Session(s.playID)
	if !ok || ps == nil || ps.Resolution == nil || ps.Resolution.Release.ID != release {
		return core.ErrNotAvailable
	}
	return nil
}

func normalizeVersionHandle(handle string) string {
	if len(handle) == 36 {
		if handle[8] != '-' || handle[13] != '-' || handle[18] != '-' || handle[23] != '-' {
			return ""
		}
		handle = strings.ReplaceAll(handle, "-", "")
	}
	if len(handle) != 32 {
		return ""
	}
	id, err := hex.DecodeString(handle)
	if err != nil || id[6]>>4 != 4 || id[8]>>6 != 2 {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}
