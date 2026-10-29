package sessions

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type versionHandles struct {
	db       *sql.DB
	broker   *Broker
	protocol frontend.Protocol
}

// NewVersionHandles creates a surface-bound, durable version identifier map.
// Handles are not credentials and all lookups recheck viewer and title policy.
func NewVersionHandles(db *sql.DB, b *Broker, protocol frontend.Protocol) (frontend.VersionHandles, error) {
	if db == nil || b == nil || protocol == "" {
		return nil, errors.New("sessions: missing version handle dependency")
	}
	return &versionHandles{db: db, broker: b, protocol: protocol}, nil
}

func (v *versionHandles) validate(ctx context.Context, scope frontend.VersionScope, item core.RatingKey, release string) (*core.Viewer, error) {
	if scope.Protocol != v.protocol || scope.Device.ID == "" || len(scope.Device.ID) > 512 || item == 0 || release == "" || len(release) > 512 || v.broker.deps.Variants == nil {
		return nil, core.ErrNotFound
	}
	viewer, err := v.broker.viewer(ctx, scope.Viewer)
	if err != nil {
		return nil, err
	}
	caps := scope.Caps
	caps.Protocol = v.protocol
	caps.ClientID = clientKey(v.protocol, scope.Device)
	if !v.broker.permitted(viewer, caps) {
		return nil, core.ErrUnauthorized
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
		return nil, core.ErrNotFound
	}
	if title.Type == core.TypeShow || title.Type == core.TypeSeason {
		return nil, core.ErrNotFound
	}
	offered, err := v.broker.deps.Variants(ctx, viewer, title, caps)
	if err != nil {
		return nil, err
	}
	for _, candidate := range offered {
		if candidate.ID == release {
			return viewer, nil
		}
	}
	return nil, core.ErrNotFound
}

func (v *versionHandles) Issue(ctx context.Context, scope frontend.VersionScope, item core.RatingKey, release string) (string, error) {
	viewer, err := v.validate(ctx, scope, item, release)
	if err != nil {
		return "", err
	}
	token, err := randomUUID()
	if err != nil {
		return "", err
	}
	_, err = v.db.ExecContext(ctx, `INSERT INTO sessions_version_handles(handle,viewer_id,surface,device_id,item_id,release_id) VALUES(?,?,?,?,?,?) ON CONFLICT(viewer_id,surface,device_id,item_id,release_id) DO NOTHING`, token, viewer.ID, string(v.protocol), scope.Device.ID, int64(item), release)
	if err != nil {
		return "", err
	}
	var stored string
	err = v.db.QueryRowContext(ctx, `SELECT handle FROM sessions_version_handles WHERE viewer_id=? AND surface=? AND device_id=? AND item_id=? AND release_id=?`, viewer.ID, string(v.protocol), scope.Device.ID, int64(item), release).Scan(&stored)
	return stored, err
}

func (v *versionHandles) Resolve(ctx context.Context, scope frontend.VersionScope, handle string) (frontend.VersionTarget, error) {
	item, _, err := v.resolveRecord(ctx, scope, handle)
	return frontend.VersionTarget{Item: item}, err
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
	err = v.db.QueryRowContext(ctx, `SELECT item_id,release_id FROM sessions_version_handles WHERE handle=? AND viewer_id=? AND surface=? AND device_id=?`, handle, viewer.ID, string(v.protocol), scope.Device.ID).Scan(&item, &release)
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
