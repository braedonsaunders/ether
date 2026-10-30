package sessions

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

const detailSourceTTL = time.Hour
const maxDetailSourcesPerViewer = 256
const maxDetailSources = 8192

type detailSourceRecord struct {
	item              core.RatingKey
	release, revision string
}

// issueDetailSource stores only an authenticated scope and the source facts
// needed to re-open the exact release on another node. Handles are not grants.
func (b *Broker) issueDetailSource(ctx context.Context, req frontend.PlanRequest, ps *core.PlaySession, etag string) (string, error) {
	if b.deps.DB == nil || req.Viewer == nil || ps == nil || ps.Resolution == nil || ps.Resolution.Release.ID == "" || len(ps.Resolution.Release.ID) > 512 || etag == "" {
		return "", nil
	}
	handle, err := randomUUID()
	if err != nil {
		return "", err
	}
	now := b.deps.Now().UnixMilli()
	tx, err := b.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(b.deps.DB), tx, "sessions:detail-source-admission"); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions_detail_sources WHERE expires<=?`, now); err != nil {
		return "", err
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT handle FROM sessions_detail_sources WHERE viewer_id=? AND surface=? AND device_id=? AND item_id=? AND revision=?`, req.Viewer.ID, string(req.Protocol), req.Device.ID, int64(req.Item), etag).Scan(&existing)
	if err == nil {
		if _, err = tx.ExecContext(ctx, `UPDATE sessions_detail_sources SET expires=? WHERE handle=?`, now+detailSourceTTL.Milliseconds(), existing); err != nil {
			return "", err
		}
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var total, viewerCount int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_detail_sources`).Scan(&total); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions_detail_sources WHERE viewer_id=? AND surface=?`, req.Viewer.ID, string(req.Protocol)).Scan(&viewerCount); err != nil {
		return "", err
	}
	if total >= maxDetailSources || viewerCount >= maxDetailSourcesPerViewer {
		return "", &frontend.SessionBusyError{RetryAfter: time.Second}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions_detail_sources(handle,viewer_id,surface,device_id,item_id,release_id,revision,expires) VALUES(?,?,?,?,?,?,?,?)`, handle, req.Viewer.ID, string(req.Protocol), req.Device.ID, int64(req.Item), ps.Resolution.Release.ID, etag, now+detailSourceTTL.Milliseconds())
	if err != nil {
		return "", err
	}
	return handle, tx.Commit()
}

func (b *Broker) resolveDetailSource(ctx context.Context, scope frontend.VersionScope, handle string) (detailSourceRecord, error) {
	var out detailSourceRecord
	if b.deps.DB == nil || scope.Viewer == nil || scope.Device.ID == "" || len(scope.Device.ID) > 512 || scope.Protocol == "" || normalizeVersionHandle(handle) == "" {
		return out, core.ErrNotFound
	}
	viewer, err := b.viewer(ctx, scope.Viewer)
	if err != nil {
		return out, err
	}
	if !b.permitted(viewer, scope.Caps) {
		return out, core.ErrUnauthorized
	}
	var item int64
	err = b.deps.DB.QueryRowContext(ctx, `SELECT item_id,release_id,revision FROM sessions_detail_sources WHERE handle=? AND viewer_id=? AND surface=? AND device_id=? AND expires>?`, normalizeVersionHandle(handle), viewer.ID, string(scope.Protocol), scope.Device.ID, b.deps.Now().UnixMilli()).Scan(&item, &out.release, &out.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return detailSourceRecord{}, frontend.ErrSourceGone
	}
	if err != nil {
		return detailSourceRecord{}, err
	}
	out.item = core.RatingKey(item)
	lookup := b.deps.MediaLookup
	if lookup == nil {
		lookup = b.deps.Lookup
	}
	title, err := lookup(ctx, viewer, scope.Device, out.item)
	if err != nil || title == nil || title.Key != out.item || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) {
		return detailSourceRecord{}, core.ErrNotFound
	}
	return out, nil
}

func (b *Broker) ResolveDetailSource(ctx context.Context, scope frontend.VersionScope, handle string) (frontend.DetailSourceTarget, error) {
	r, err := b.resolveDetailSource(ctx, scope, handle)
	if err != nil {
		return frontend.DetailSourceTarget{}, err
	}
	return frontend.DetailSourceTarget{Item: r.item, ETag: r.revision}, nil
}

// MatchDetailSession authorizes an existing session only when it is pinned to
// the offered source and the authenticated viewer/device/surface still match.
func (b *Broker) MatchDetailSession(ctx context.Context, scope frontend.VersionScope, handle, sessionID string) error {
	record, err := b.resolveDetailSource(ctx, scope, handle)
	if err != nil {
		return err
	}
	s, _, err := b.access(ctx, frontend.SessionAccess{Viewer: scope.Viewer, Device: scope.Device, Protocol: scope.Protocol}, sessionID)
	if err != nil {
		return err
	}
	if s.public.Item != record.item || !s.sourcePinned || s.sourceETag != record.revision {
		return frontend.ErrSourceGone
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil || ps.Resolution == nil || ps.Resolution.Release.ID != record.release || sourceRevision(ps) != record.revision {
		return frontend.ErrSourceGone
	}
	return nil
}

// localDetailSource finds a still-leased source for the exact authenticated
// scope; callers hold that scope's owner gate throughout validation and use.
func (b *Broker) localDetailSource(viewer int64, device string, surface frontend.Protocol, item core.RatingKey, record detailSourceRecord) *core.PlaySession {
	b.mu.Lock()
	var ids []string
	for _, p := range b.plans {
		if p.request.Viewer != nil && p.request.Viewer.ID == viewer && p.request.Device.ID == device && p.request.Protocol == surface && p.request.Item == item && p.source != nil {
			ids = append(ids, p.source.ID)
		}
	}
	for _, s := range b.sessions {
		if s.public.ViewerID == viewer && s.public.Device.ID == device && s.public.Protocol == surface && s.public.Item == item {
			ids = append(ids, s.playID)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		ps, ok := b.deps.Playback.Session(id)
		if ok && ps != nil && ps.Resolution != nil && ps.Resolution.Release.ID == record.release && sourceRevision(ps) == record.revision {
			return ps
		}
	}
	return nil
}
