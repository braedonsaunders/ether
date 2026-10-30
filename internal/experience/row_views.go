package experience

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

func init() {
	store.Register("experience_row_views", 1, `CREATE TABLE experience_row_views (id TEXT PRIMARY KEY, scope_hash TEXT NOT NULL UNIQUE, viewer_id INTEGER NOT NULL, device_id TEXT NOT NULL, protocol TEXT NOT NULL, snapshot TEXT NOT NULL, expires INTEGER NOT NULL, payload TEXT NOT NULL); CREATE INDEX experience_row_views_viewer ON experience_row_views(viewer_id,device_id);`)
	store.Register("experience_row_views", 2, `CREATE TABLE experience_row_view_manifests (scope_hash TEXT PRIMARY KEY, viewer_id INTEGER NOT NULL, device_id TEXT NOT NULL, protocol TEXT NOT NULL, generation TEXT NOT NULL, expires INTEGER NOT NULL, payload TEXT NOT NULL);`)
	store.Register("experience_row_views", 3, `CREATE UNIQUE INDEX experience_row_views_snapshot ON experience_row_views(snapshot);`)
}

var ErrRowViewNotFound = fmt.Errorf("experience: row view unavailable: %w", frontend.ErrNotFound)
var ErrRowViewStale = frontend.ErrRowViewStale

// RowViewSnapshot is produced by the trusted row ranker after policy and
// layout. Entries are identities only; ResolveEntry must rehydrate them.
type RowViewSnapshot struct {
	Row       frontend.Row
	Situation core.HomeSituation
	Placement frontend.Placement
	ViewID    string
	Item      core.RatingKey
	Entries   []core.NeutralEntryRef
	Truncated bool
	Revision  string
}

type RowViewOptions struct {
	DB     *sql.DB
	TTL    time.Duration
	Now    func() time.Time
	Active func(context.Context, int64) (*core.User, error)
	Build  func(context.Context, frontend.RowViewRequest) ([]RowViewSnapshot, error)
	// ValidRow checks current placement, collection and viewer preference scope,
	// including the snapshot revision; it must fail closed on uncertainty.
	ValidRow     func(context.Context, *core.User, core.Device, core.Protocol, RowViewSnapshot) bool
	ResolveEntry func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error)
	// ResolveEntries may batch rehydration; every call must perform fresh policy checks.
	ResolveEntries func(context.Context, *core.User, core.Device, core.Protocol, []core.NeutralEntryRef) ([]frontend.RowEntry, error)
	Receipts       *Receipts
}

func (s *RowViewService) resolveAll(ctx context.Context, u *core.User, device core.Device, protocol core.Protocol, refs []core.NeutralEntryRef) ([]frontend.RowEntry, error) {
	var entries []frontend.RowEntry
	if s.o.ResolveEntries != nil {
		var err error
		entries, err = s.o.ResolveEntries(ctx, u, device, protocol, refs)
		if err != nil || len(entries) != len(refs) {
			return nil, ErrRowViewStale
		}
	} else {
		entries = make([]frontend.RowEntry, 0, len(refs))
		for _, ref := range refs {
			entry, err := s.o.ResolveEntry(ctx, u, device, protocol, ref)
			if err != nil {
				return nil, ErrRowViewStale
			}
			entries = append(entries, entry)
		}
	}
	for i, ref := range refs {
		if string(entries[i].Kind) != ref.Kind || entryIdentity(entries[i]) != ref.ID {
			return nil, ErrRowViewStale
		}
	}
	return entries, nil
}

type RowViewService struct{ o RowViewOptions }

func NewRowViewService(o RowViewOptions) (*RowViewService, error) {
	if o.DB == nil || o.TTL < time.Second || o.TTL > 24*time.Hour || o.Active == nil || o.Build == nil || o.ValidRow == nil || o.ResolveEntry == nil || o.Receipts == nil {
		return nil, errors.New("experience: incomplete row view dependencies")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &RowViewService{o: o}, nil
}

func rowViewToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func normalizeRowViewID(id string) string {
	if len(id) == 36 {
		if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			return ""
		}
		id = strings.ReplaceAll(id, "-", "")
	}
	if len(id) != 32 {
		return ""
	}
	raw, err := hex.DecodeString(id)
	if err != nil || raw[6]>>4 != 4 || raw[8]>>6 != 2 {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func (s *RowViewService) viewer(ctx context.Context, v *core.User, d core.Device, p core.Protocol) (*core.User, error) {
	if v == nil || v.ID <= 0 || d.ID == "" || len(d.ID) > 512 || p == "" || len(p) > 128 {
		return nil, ErrRowViewNotFound
	}
	u, err := s.o.Active(ctx, v.ID)
	if err != nil || u == nil || u.ID != v.ID {
		return nil, ErrRowViewNotFound
	}
	return u, nil
}

func (s *RowViewService) ListRowViews(ctx context.Context, req frontend.RowViewRequest) ([]frontend.RowView, error) {
	u, err := s.viewer(ctx, req.Viewer, req.Device, req.Protocol)
	if err != nil {
		return nil, err
	}
	req.Viewer = u
	scopeBytes, err := json.Marshal([]any{u.ID, req.Device.ID, req.Protocol, req.Placement, req.ViewID, req.Item})
	if err != nil {
		return nil, err
	}
	scopeHash := sha256.Sum256(scopeBytes)
	manifestScope := hex.EncodeToString(scopeHash[:])
	if !req.Refresh {
		views, found, err := s.loadManifest(ctx, req, manifestScope)
		if err != nil {
			return nil, err
		}
		if found {
			return views, nil
		}
	}
	rows, err := s.o.Build(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(rows) > 100 {
		return nil, errors.New("experience: too many row views")
	}
	type candidate struct{ scope, payload string }
	candidates := make([]candidate, 0, len(rows))
	seen := map[string]bool{}
	for _, snap := range rows {
		if snap.Row.ID == "" || snap.Row.Kind == "" || len(snap.Entries) == 0 || len(snap.Entries) > 1000 || snap.Placement != req.Placement || snap.ViewID != req.ViewID || snap.Item != req.Item || seen[snap.Row.ID] || !s.o.ValidRow(ctx, u, req.Device, req.Protocol, snap) {
			return nil, ErrRowViewNotFound
		}
		seen[snap.Row.ID] = true
		seenEntries := map[core.NeutralEntryRef]bool{}
		for _, ref := range snap.Entries {
			if ref.ID == "" || seenEntries[ref] {
				return nil, ErrRowViewNotFound
			}
			seenEntries[ref] = true
		}
		if _, err := s.resolveAll(ctx, u, req.Device, req.Protocol, snap.Entries); err != nil {
			return nil, ErrRowViewNotFound
		}
		snap.Row.Entries, snap.Row.Items = nil, nil
		snap.Row.Handle, snap.Row.ImpressionID = "", ""
		payload, err := json.Marshal(snap)
		if err != nil || len(payload) > 65536 {
			return nil, errors.New("experience: invalid row snapshot")
		}
		scope, err := json.Marshal([]any{u.ID, req.Device.ID, req.Protocol, req.Placement, req.ViewID, req.Item, snap.Row.ID})
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(scope)
		scopeID := hex.EncodeToString(hash[:])
		candidates = append(candidates, candidate{scope: scopeID, payload: string(payload)})
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.o.DB), tx, fmt.Sprintf("row-view:%d:%s", u.ID, req.Device.ID)); err != nil {
		return nil, err
	}
	now := s.o.Now().UnixMilli()
	var expires int64
	var oldPayload string
	err = tx.QueryRowContext(ctx, `SELECT expires,payload FROM experience_row_view_manifests WHERE scope_hash=?`, manifestScope).Scan(&expires, &oldPayload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	exists := err == nil
	if err == nil && expires > now && !req.Refresh {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		views, found, err := s.loadManifest(ctx, req, manifestScope)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrRowViewStale
		}
		return views, nil
	}
	if oldPayload != "" {
		var old []manifestRow
		if err = json.Unmarshal([]byte(oldPayload), &old); err != nil {
			return nil, err
		}
		for _, ref := range old {
			if _, err = tx.ExecContext(ctx, `UPDATE experience_row_views SET expires=0 WHERE id=? AND snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, ref.ID, ref.Snapshot, u.ID, req.Device.ID, string(req.Protocol)); err != nil {
				return nil, err
			}
		}
	}
	// Expired visits cannot be resolved and must not exhaust the per-device
	// bound as viewers encounter new rows over time.
	if _, err = tx.ExecContext(ctx, `DELETE FROM experience_row_views WHERE viewer_id=? AND device_id=? AND expires<=?`, u.ID, req.Device.ID, now); err != nil {
		return nil, err
	}
	if !exists {
		if _, err = tx.ExecContext(ctx, `DELETE FROM experience_row_view_manifests WHERE viewer_id=? AND device_id=? AND expires<=?`, u.ID, req.Device.ID, now); err != nil {
			return nil, err
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_row_view_manifests WHERE viewer_id=? AND device_id=?`, u.ID, req.Device.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= 1000 {
			return nil, errors.New("experience: row view manifest limit reached")
		}
	}
	refs := make([]manifestRow, 0, len(candidates))
	for _, c := range candidates {
		var id string
		err = tx.QueryRowContext(ctx, `SELECT id FROM experience_row_views WHERE scope_hash=?`, c.scope).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_row_views WHERE viewer_id=? AND device_id=?`, u.ID, req.Device.ID).Scan(&count); err != nil {
				return nil, err
			}
			if count >= 1000 {
				return nil, errors.New("experience: row view limit reached")
			}
			id, err = rowViewToken()
			if err != nil {
				return nil, err
			}
			generation, e := rowViewToken()
			if e != nil {
				return nil, e
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO experience_row_views(id,scope_hash,viewer_id,device_id,protocol,snapshot,expires,payload) VALUES(?,?,?,?,?,?,?,?)`, id, c.scope, u.ID, req.Device.ID, string(req.Protocol), generation, now+s.o.TTL.Milliseconds(), c.payload)
			if err != nil {
				return nil, err
			}
			refs = append(refs, manifestRow{ID: id, Snapshot: generation})
		} else if err != nil {
			return nil, err
		} else {
			generation, e := rowViewToken()
			if e != nil {
				return nil, e
			}
			_, err = tx.ExecContext(ctx, `UPDATE experience_row_views SET snapshot=?,expires=?,payload=? WHERE id=?`, generation, now+s.o.TTL.Milliseconds(), c.payload, id)
			if err != nil {
				return nil, err
			}
			refs = append(refs, manifestRow{ID: id, Snapshot: generation})
		}
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	generation, err := rowViewToken()
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO experience_row_view_manifests(scope_hash,viewer_id,device_id,protocol,generation,expires,payload) VALUES(?,?,?,?,?,?,?) ON CONFLICT(scope_hash) DO UPDATE SET generation=excluded.generation,expires=excluded.expires,payload=excluded.payload`, manifestScope, u.ID, req.Device.ID, string(req.Protocol), generation, now+s.o.TTL.Milliseconds(), string(b))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	views, found, err := s.loadManifest(ctx, req, manifestScope)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrRowViewStale
	}
	return views, nil
}

type manifestRow struct{ ID, Snapshot string }

// loadManifest returns the shared ordered visit, revalidating every row and
// entry. A revoked member invalidates the visit instead of shifting offsets.
func (s *RowViewService) loadManifest(ctx context.Context, req frontend.RowViewRequest, scope string) ([]frontend.RowView, bool, error) {
	var generation, payload string
	var expires int64
	err := s.o.DB.QueryRowContext(ctx, `SELECT generation,expires,payload FROM experience_row_view_manifests WHERE scope_hash=? AND viewer_id=? AND device_id=? AND protocol=?`, scope, req.Viewer.ID, req.Device.ID, string(req.Protocol)).Scan(&generation, &expires, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if expires <= s.o.Now().UnixMilli() {
		return nil, false, nil
	}
	var refs []manifestRow
	if err = json.Unmarshal([]byte(payload), &refs); err != nil || len(refs) > 100 {
		return nil, false, ErrRowViewStale
	}
	out := make([]frontend.RowView, 0, len(refs))
	snapshots := make([]RowViewSnapshot, 0, len(refs))
	for _, ref := range refs {
		var raw string
		var expiry int64
		err = s.o.DB.QueryRowContext(ctx, `SELECT payload,expires FROM experience_row_views WHERE id=? AND snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, ref.ID, ref.Snapshot, req.Viewer.ID, req.Device.ID, string(req.Protocol)).Scan(&raw, &expiry)
		if err != nil || expiry <= s.o.Now().UnixMilli() {
			return nil, false, ErrRowViewStale
		}
		var snap RowViewSnapshot
		if json.Unmarshal([]byte(raw), &snap) != nil || snap.Placement != req.Placement || snap.ViewID != req.ViewID || snap.Item != req.Item || !s.o.ValidRow(ctx, req.Viewer, req.Device, req.Protocol, snap) {
			return nil, false, ErrRowViewStale
		}
		if _, err := s.resolveAll(ctx, req.Viewer, req.Device, req.Protocol, snap.Entries); err != nil {
			return nil, false, ErrRowViewStale
		}
		out = append(out, frontend.RowView{ID: ref.Snapshot, Title: snap.Row.Title, Kind: snap.Row.Kind, Purpose: snap.Row.Purpose, Layout: snap.Row.Layout, Rank: snap.Row.Rank})
		snapshots = append(snapshots, snap)
	}
	var current string
	var currentExpiry int64
	if err = s.o.DB.QueryRowContext(ctx, `SELECT generation,expires FROM experience_row_view_manifests WHERE scope_hash=? AND viewer_id=? AND device_id=? AND protocol=?`, scope, req.Viewer.ID, req.Device.ID, string(req.Protocol)).Scan(&current, &currentExpiry); err != nil || current != generation || currentExpiry <= s.o.Now().UnixMilli() {
		return nil, false, ErrRowViewStale
	}
	active, err := s.viewer(ctx, req.Viewer, req.Device, req.Protocol)
	if err != nil {
		return nil, false, ErrRowViewNotFound
	}
	for _, snap := range snapshots {
		if !s.o.ValidRow(ctx, active, req.Device, req.Protocol, snap) {
			return nil, false, ErrRowViewStale
		}
		if _, err := s.resolveAll(ctx, active, req.Device, req.Protocol, snap.Entries); err != nil {
			return nil, false, ErrRowViewStale
		}
	}
	// A refresh can commit while the final policy pass is running.
	if err = s.o.DB.QueryRowContext(ctx, `SELECT generation,expires FROM experience_row_view_manifests WHERE scope_hash=? AND viewer_id=? AND device_id=? AND protocol=?`, scope, req.Viewer.ID, req.Device.ID, string(req.Protocol)).Scan(&current, &currentExpiry); err != nil || current != generation || currentExpiry <= s.o.Now().UnixMilli() {
		return nil, false, ErrRowViewStale
	}
	return out, true, nil
}

func (s *RowViewService) ResolveRowView(ctx context.Context, req frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	var page frontend.RowViewPage
	req.ID = normalizeRowViewID(req.ID)
	u, err := s.viewer(ctx, req.Viewer, req.Device, req.Protocol)
	if err != nil {
		return page, err
	}
	if req.ID == "" || req.Offset < 0 || req.Limit <= 0 || req.Limit > 100 {
		return page, ErrRowViewNotFound
	}
	var generation, payload string
	var expires int64
	err = s.o.DB.QueryRowContext(ctx, `SELECT snapshot,expires,payload FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, req.ID, u.ID, req.Device.ID, string(req.Protocol)).Scan(&generation, &expires, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return page, ErrRowViewNotFound
	}
	if err != nil {
		return page, err
	}
	if generation != req.ID || expires <= s.o.Now().UnixMilli() {
		return page, ErrRowViewStale
	}
	var snap RowViewSnapshot
	if err = json.Unmarshal([]byte(payload), &snap); err != nil {
		return page, err
	}
	if !s.o.ValidRow(ctx, u, req.Device, req.Protocol, snap) {
		return page, ErrRowViewStale
	}
	allowedKinds := map[frontend.RowEntryKind]bool{}
	for _, kind := range req.Kinds {
		allowedKinds[kind] = true
	}
	resolved, err := s.resolveAll(ctx, u, req.Device, req.Protocol, snap.Entries)
	if err != nil {
		return page, ErrRowViewStale
	}
	var entries []frontend.RowEntry
	for _, entry := range resolved {
		if len(allowedKinds) == 0 || allowedKinds[entry.Kind] {
			entry.Handle = ""
			entries = append(entries, entry)
		}
	}
	page.Total, page.TotalExact, page.Truncated = len(entries), !snap.Truncated, snap.Truncated
	start := min(req.Offset, len(entries))
	end := min(start+req.Limit, len(entries))
	page.More = end < len(entries)
	page.Entries = entries[start:end]
	current := func() bool {
		var generation string
		var expiry int64
		err := s.o.DB.QueryRowContext(ctx, `SELECT snapshot,expires FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, req.ID, u.ID, req.Device.ID, string(req.Protocol)).Scan(&generation, &expiry)
		active, activeErr := s.viewer(ctx, u, req.Device, req.Protocol)
		if err != nil || activeErr != nil || generation != req.ID || expiry <= s.o.Now().UnixMilli() || !s.o.ValidRow(ctx, active, req.Device, req.Protocol, snap) {
			return false
		}
		_, err = s.resolveAll(ctx, active, req.Device, req.Protocol, snap.Entries)
		if err != nil {
			return false
		}
		err = s.o.DB.QueryRowContext(ctx, `SELECT snapshot,expires FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, req.ID, u.ID, req.Device.ID, string(req.Protocol)).Scan(&generation, &expiry)
		return err == nil && generation == req.ID && expiry > s.o.Now().UnixMilli()
	}
	if len(page.Entries) == 0 {
		if !current() {
			return frontend.RowViewPage{}, ErrRowViewStale
		}
		return page, nil
	}
	imp := &core.NeutralImpression{RowID: snap.Row.ID, Kind: snap.Row.Kind, Title: snap.Row.Title, Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Situation: snap.Situation, Collection: snap.Row.Collection, Placement: string(snap.Placement), ViewID: snap.ViewID, Item: snap.Item}
	for _, e := range page.Entries {
		imp.Entries = append(imp.Entries, core.NeutralEntryRef{Kind: string(e.Kind), ID: entryIdentity(e)})
		if e.Title != nil {
			imp.Items = append(imp.Items, e.Title.Key)
		}
	}
	subjects := make([]ReceiptSubject, 0, len(page.Entries)+1)
	subjects = append(subjects, ReceiptSubject{Kind: "row", ID: snap.Row.ID, Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Impression: imp})
	for i := range page.Entries {
		e := &page.Entries[i]
		subjects = append(subjects, ReceiptSubject{Kind: string(e.Kind), ID: fmt.Sprintf("%s/%s/%s", snap.Row.ID, e.Kind, entryIdentity(*e)), Purpose: snap.Row.Purpose, Rank: snap.Row.Rank, Impression: imp})
	}
	tokens, err := s.o.Receipts.issueBatchChecked(ctx, u, req.Device, req.Protocol, subjects, current, func(tx *sql.Tx) error {
		var expiry int64
		if err := tx.QueryRowContext(ctx, `SELECT expires FROM experience_row_views WHERE snapshot=? AND viewer_id=? AND device_id=? AND protocol=?`, req.ID, u.ID, req.Device.ID, string(req.Protocol)).Scan(&expiry); err != nil || expiry <= s.o.Now().UnixMilli() {
			return ErrRowViewStale
		}
		return nil
	})
	if errors.Is(err, viewerpolicy.ErrChanged) {
		return frontend.RowViewPage{}, ErrRowViewStale
	}
	if err != nil {
		return frontend.RowViewPage{}, err
	}
	page.ImpressionHandle = tokens[0]
	for i := range page.Entries {
		page.Entries[i].Handle = tokens[i+1]
	}
	return page, nil
}

var _ frontend.RowViews = (*RowViewService)(nil)
