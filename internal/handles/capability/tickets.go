// Package capability contains opt-in shared capability storage. The legacy
// ticket service remains authoritative until mixed-version cutover is proven.
package capability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("capability_headers", "auth", "identity_grants")
	store.Register("capability_headers", 1, `CREATE TABLE capability_headers (namespace TEXT NOT NULL,digest TEXT NOT NULL,shard INTEGER NOT NULL,viewer_id BIGINT NOT NULL,surface TEXT NOT NULL,device_id TEXT NOT NULL,expires BIGINT NOT NULL,PRIMARY KEY(namespace,digest)); CREATE INDEX capability_headers_expiry ON capability_headers(namespace,shard,expires); CREATE INDEX capability_headers_scope ON capability_headers(namespace,shard,viewer_id,surface,expires); CREATE TABLE capability_tickets (namespace TEXT NOT NULL,digest TEXT NOT NULL,purpose TEXT NOT NULL,source_hash TEXT NOT NULL REFERENCES identity_grants(hash) ON DELETE CASCADE,device TEXT NOT NULL,legacy_mirror INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(namespace,digest),FOREIGN KEY(namespace,digest) REFERENCES capability_headers(namespace,digest) ON DELETE CASCADE);`)
}

const ticketNamespace = "ticket"
const ticketShards int64 = 64
const ticketPerShard = 256
const ticketPerScope = 128
const ticketBatchMax = 40
const ticketCleanupMax = 128
const ticketTTL = 30 * time.Second

type Tickets struct {
	db    *sql.DB
	clock func() time.Time
	// Only set by package-local tests before concurrent use.
	beforeConsumeLock func()
}
type TicketScope struct {
	ViewerID                                  int64
	Surface, DeviceID, SourceHash, DeviceJSON string
}
type TicketEntry struct{ Digest, Purpose string }
type Ticket struct {
	ViewerID               int64
	DeviceJSON, SourceHash string
}
type CapacityError struct{ Scope string }

func (e *CapacityError) Error() string {
	return "capability: ticket capacity reached (" + e.Scope + ")"
}
func NewTickets(db *sql.DB) (*Tickets, error) {
	if db == nil {
		return nil, errors.New("capability: database required")
	}
	return &Tickets{db: db}, nil
}

// NewTicketsWithClock preserves a service-owned trusted clock contract. The
// clock is injected by the host, never by a front-end request.
func NewTicketsWithClock(db *sql.DB, clock func() time.Time) (*Tickets, error) {
	if db == nil || clock == nil {
		return nil, errors.New("capability: database and clock required")
	}
	return &Tickets{db: db, clock: clock}, nil
}
func (s *Tickets) now(ctx context.Context, tx *sql.Tx) (int64, error) {
	if s.clock != nil {
		return s.clock().UnixMilli(), nil
	}
	return dbTime(ctx, s.db, tx)
}
func digestValid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
func (s TicketScope) valid() bool {
	if s.ViewerID <= 0 || s.Surface == "" || len(s.Surface) > 128 || s.DeviceID == "" || len(s.DeviceID) > 512 || !digestValid(s.SourceHash) || s.DeviceJSON == "" || len(s.DeviceJSON) > 4096 {
		return false
	}
	var device core.Device
	return json.Unmarshal([]byte(s.DeviceJSON), &device) == nil && device.ID == s.DeviceID
}
func dbTime(ctx context.Context, db *sql.DB, tx *sql.Tx) (int64, error) {
	query := `SELECT CAST((julianday('now') - 2440587.5)*86400000 + 0.999 AS INTEGER)`
	if store.DialectOf(db) == store.Postgres {
		query = `SELECT CAST(CEIL(EXTRACT(EPOCH FROM clock_timestamp())*1000) AS BIGINT)`
	}
	var now int64
	err := tx.QueryRowContext(ctx, query).Scan(&now)
	return now, err
}
func (s *Tickets) lock(ctx context.Context, tx *sql.Tx, viewer int64) error {
	return store.LockKey(ctx, store.DialectOf(s.db), tx, fmt.Sprintf("capability:ticket:%d", viewer%ticketShards))
}

// IssueBatch inserts distinct pre-hashed opaque ticket identifiers. It never
// accepts a caller-supplied timestamp or an authorization decision: the source
// grant and active viewer are checked in SQL inside the insertion transaction.
// Existing legacy ticket rows remain separate and are never migrated or removed.
func (s *Tickets) IssueBatch(ctx context.Context, scope TicketScope, entries []TicketEntry) (int64, error) {
	return s.issueBatch(ctx, scope, entries, false)
}

// IssueMirrored atomically creates the shared header and legacy-authoritative
// ticket under the legacy-authoritative transition.
func (s *Tickets) IssueMirrored(ctx context.Context, scope TicketScope, entries []TicketEntry) (int64, error) {
	return s.issueBatch(ctx, scope, entries, true)
}

func (s *Tickets) issueBatch(ctx context.Context, scope TicketScope, entries []TicketEntry, mirror bool) (int64, error) {
	if !scope.valid() || len(entries) == 0 || len(entries) > ticketBatchMax {
		return 0, core.ErrUnauthorized
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !digestValid(e.Digest) || e.Purpose == "" || len(e.Purpose) > 64 || seen[e.Digest] {
			return 0, core.ErrUnauthorized
		}
		seen[e.Digest] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// All upgraded legacy cleanup takes the global lock before its shard.
	// Old binaries do not take it; their SQL still relies on row-level safety.
	if mirror {
		if err = store.LockKey(ctx, store.DialectOf(s.db), tx, "capability:legacy-ticket-cleanup"); err != nil {
			return 0, err
		}
	}
	if err = s.lock(ctx, tx, scope.ViewerID); err != nil {
		return 0, err
	}
	now, err := s.now(ctx, tx)
	if err != nil {
		return 0, err
	}
	shard := scope.ViewerID % ticketShards
	if mirror {
		// Legacy remains authoritative. Bound expired-only cleanup; never
		// remove an unexpired ticket. This shares the issue transaction.
		if _, err = tx.ExecContext(ctx, `DELETE FROM frontend_tickets WHERE expires<=? AND hash IN (SELECT hash FROM frontend_tickets WHERE expires<=? ORDER BY expires,hash LIMIT ?)`, now, now, ticketCleanupMax); err != nil {
			return 0, err
		}
	}
	// Bounded cleanup of this namespace/shard only. Cascades remove typed rows.
	if _, err = tx.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND expires<=? AND digest IN (SELECT digest FROM capability_headers WHERE namespace=? AND shard=? AND expires<=? ORDER BY expires,digest LIMIT ?)`, ticketNamespace, now, ticketNamespace, shard, now, ticketCleanupMax); err != nil {
		return 0, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND shard=? AND viewer_id=? AND surface=?`, ticketNamespace, shard, scope.ViewerID, scope.Surface).Scan(&count); err != nil {
		return 0, err
	}
	if count+len(entries) > ticketPerScope {
		return 0, &CapacityError{Scope: "viewer"}
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace=? AND shard=?`, ticketNamespace, shard).Scan(&count); err != nil {
		return 0, err
	}
	if count+len(entries) > ticketPerShard {
		return 0, &CapacityError{Scope: "shard"}
	}
	expiry := now + ticketTTL.Milliseconds()
	// One guarded header INSERT and one typed-child INSERT per bounded batch.
	var header strings.Builder
	header.WriteString(`INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) `)
	var args []any
	for i, e := range entries {
		if i > 0 {
			header.WriteString(` UNION ALL `)
		}
		header.WriteString(`SELECT CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT),CAST(? AS BIGINT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT) WHERE EXISTS(SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=CAST(? AS TEXT) AND g.surface=CAST(? AS TEXT) AND g.viewer_id=CAST(? AS BIGINT) AND g.device_id=CAST(? AS TEXT) AND g.revoked=0 AND g.expires>CAST(? AS BIGINT) AND u.active=1)`)
		args = append(args, ticketNamespace, e.Digest, shard, scope.ViewerID, scope.Surface, scope.DeviceID, expiry, scope.SourceHash, scope.Surface, scope.ViewerID, scope.DeviceID, now)
	}
	res, err := tx.ExecContext(ctx, header.String(), args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != int64(len(entries)) {
		return 0, core.ErrUnauthorized
	}
	var child strings.Builder
	child.WriteString(`INSERT INTO capability_tickets(namespace,digest,purpose,source_hash,device,legacy_mirror) VALUES `)
	args = nil
	for i, e := range entries {
		if i > 0 {
			child.WriteString(",")
		}
		child.WriteString("(?,?,?,?,?,?)")
		mirrorFlag := 0
		if mirror {
			mirrorFlag = 1
		}
		args = append(args, ticketNamespace, e.Digest, e.Purpose, scope.SourceHash, scope.DeviceJSON, mirrorFlag)
	}
	if _, err = tx.ExecContext(ctx, child.String(), args...); err != nil {
		return 0, err
	}
	if mirror {
		// Keep the old table authoritative. The mirrored legacy row and shared
		// header either both commit or neither does. Recheck the source grant.
		var legacy strings.Builder
		legacy.WriteString(`INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) `)
		args = nil
		for i, e := range entries {
			if i > 0 {
				legacy.WriteString(` UNION ALL `)
			}
			legacy.WriteString(`SELECT CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS BIGINT) WHERE EXISTS(SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=CAST(? AS TEXT) AND g.surface=CAST(? AS TEXT) AND g.viewer_id=CAST(? AS BIGINT) AND g.device_id=CAST(? AS TEXT) AND g.revoked=0 AND g.expires>CAST(? AS BIGINT) AND u.active=1)`)
			args = append(args, e.Digest, scope.Surface, e.Purpose, scope.ViewerID, scope.SourceHash, scope.DeviceJSON, expiry, scope.SourceHash, scope.Surface, scope.ViewerID, scope.DeviceID, now)
		}
		result, e := tx.ExecContext(ctx, legacy.String(), args...)
		if e != nil {
			return 0, e
		}
		rows, e := result.RowsAffected()
		if e != nil {
			return 0, e
		}
		if rows != int64(len(entries)) {
			return 0, core.ErrUnauthorized
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return expiry, nil
}

// Consume uses a guarded DELETE as the linearization point; selection alone
// never authorizes consumption. Legacy fallback is not yet implemented.
func (s *Tickets) Consume(ctx context.Context, surface, purpose, digest string) (Ticket, error) {
	var out Ticket
	if surface == "" || purpose == "" || !digestValid(digest) {
		return out, core.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var deviceID, storedSurface string
	var storedPurpose, storedDevice, storedSource sql.NullString
	var storedMirror sql.NullInt64
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT h.viewer_id,h.device_id,h.expires,t.device,t.source_hash,h.surface,t.purpose,t.legacy_mirror FROM capability_headers h LEFT JOIN capability_tickets t ON t.namespace=h.namespace AND t.digest=h.digest WHERE h.namespace=? AND h.digest=?`, ticketNamespace, digest).Scan(&out.ViewerID, &deviceID, &expires, &storedDevice, &storedSource, &storedSurface, &storedPurpose, &storedMirror)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, sql.ErrNoRows
	}
	if err != nil {
		return Ticket{}, err
	}
	if !storedPurpose.Valid || !storedDevice.Valid || !storedSource.Valid || !storedMirror.Valid || storedMirror.Int64 != 0 || storedSurface != surface || storedPurpose.String != purpose {
		return Ticket{}, core.ErrUnauthorized
	}
	initialViewer := out.ViewerID
	if s.beforeConsumeLock != nil {
		s.beforeConsumeLock()
	}
	if err = s.lock(ctx, tx, initialViewer); err != nil {
		return Ticket{}, err
	}
	// Re-read after the lock: the pre-lock identity may have been deleted and
	// reissued. Never use fields read before the lock as authority.
	err = tx.QueryRowContext(ctx, `SELECT h.viewer_id,h.device_id,h.expires,t.device,t.source_hash,h.surface,t.purpose,t.legacy_mirror FROM capability_headers h LEFT JOIN capability_tickets t ON t.namespace=h.namespace AND t.digest=h.digest WHERE h.namespace=? AND h.digest=?`, ticketNamespace, digest).Scan(&out.ViewerID, &deviceID, &expires, &storedDevice, &storedSource, &storedSurface, &storedPurpose, &storedMirror)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, core.ErrUnauthorized
	}
	if err != nil {
		return Ticket{}, err
	}
	if out.ViewerID != initialViewer || !storedPurpose.Valid || !storedDevice.Valid || !storedSource.Valid || !storedMirror.Valid || storedMirror.Int64 != 0 || storedSurface != surface || storedPurpose.String != purpose {
		return Ticket{}, core.ErrUnauthorized
	}
	out.DeviceJSON, out.SourceHash = storedDevice.String, storedSource.String
	var device core.Device
	if json.Unmarshal([]byte(out.DeviceJSON), &device) != nil || device.ID != deviceID {
		return Ticket{}, core.ErrUnauthorized
	}
	now, err := s.now(ctx, tx)
	if err != nil {
		return Ticket{}, err
	}
	var removed string
	err = tx.QueryRowContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND digest=? AND viewer_id=? AND surface=? AND device_id=? AND expires>? AND EXISTS(SELECT 1 FROM capability_tickets t JOIN identity_grants g ON g.hash=t.source_hash JOIN auth_users u ON u.id=g.viewer_id WHERE t.namespace=capability_headers.namespace AND t.digest=capability_headers.digest AND t.purpose=? AND g.viewer_id=capability_headers.viewer_id AND g.surface=capability_headers.surface AND g.device_id=capability_headers.device_id AND g.revoked=0 AND g.expires>? AND u.active=1) RETURNING digest`, ticketNamespace, digest, out.ViewerID, surface, deviceID, now, purpose, now).Scan(&removed)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, core.ErrUnauthorized
	}
	if err != nil {
		return Ticket{}, err
	}
	if err = tx.Commit(); err != nil {
		return Ticket{}, err
	}
	return out, nil
}

// ConsumeMirrored treats frontend_tickets as the sole consume authority. It
// does not consult an absent-header fallback. Shared metadata, when present, is
// removed in the same transaction.
func (s *Tickets) ConsumeMirrored(ctx context.Context, surface, purpose, digest string) (Ticket, error) {
	var out Ticket
	if surface == "" || purpose == "" || !digestValid(digest) {
		return out, core.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var initialViewer int64
	var initialDevice, initialSource string
	err = tx.QueryRowContext(ctx, `SELECT viewer_id,device,source_hash FROM frontend_tickets WHERE hash=? AND surface=? AND purpose=?`, digest, surface, purpose).Scan(&initialViewer, &initialDevice, &initialSource)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, core.ErrUnauthorized
	}
	if err != nil {
		return Ticket{}, err
	}
	var device core.Device
	if json.Unmarshal([]byte(initialDevice), &device) != nil || device.ID == "" {
		return Ticket{}, core.ErrUnauthorized
	}
	// Acquire shard before either legacy or header mutation; this matches issue.
	if s.beforeConsumeLock != nil {
		s.beforeConsumeLock()
	}
	if err = s.lock(ctx, tx, initialViewer); err != nil {
		return Ticket{}, err
	}
	now, err := s.now(ctx, tx)
	if err != nil {
		return Ticket{}, err
	}
	err = tx.QueryRowContext(ctx, `DELETE FROM frontend_tickets WHERE hash=? AND surface=? AND purpose=? AND viewer_id=? AND source_hash=? AND device=? AND expires>? AND EXISTS(SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=frontend_tickets.source_hash AND g.viewer_id=frontend_tickets.viewer_id AND g.surface=frontend_tickets.surface AND g.device_id=? AND g.revoked=0 AND g.expires>? AND u.active=1) RETURNING viewer_id,device,source_hash`, digest, surface, purpose, initialViewer, initialSource, initialDevice, now, device.ID, now).Scan(&out.ViewerID, &out.DeviceJSON, &out.SourceHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, core.ErrUnauthorized
	}
	if err != nil {
		return Ticket{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND digest=? AND viewer_id=? AND surface=? AND device_id=? AND EXISTS(SELECT 1 FROM capability_tickets t WHERE t.namespace=capability_headers.namespace AND t.digest=capability_headers.digest AND t.legacy_mirror=1 AND t.purpose=? AND t.source_hash=? AND t.device=?)`, ticketNamespace, digest, out.ViewerID, surface, device.ID, purpose, out.SourceHash, out.DeviceJSON); err != nil {
		return Ticket{}, err
	}
	if err = tx.Commit(); err != nil {
		return Ticket{}, err
	}
	return out, nil
}
