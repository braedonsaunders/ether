// Package capability contains shared capability storage.
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
	store.Register("capability_headers", 3, `CREATE TABLE capability_claims(namespace TEXT NOT NULL,digest TEXT NOT NULL,purpose TEXT NOT NULL,PRIMARY KEY(namespace,digest,purpose),FOREIGN KEY(namespace,digest) REFERENCES capability_headers(namespace,digest) ON DELETE CASCADE);`)
	store.Register("capability_headers", 2, `CREATE INDEX capability_headers_maintenance ON capability_headers(namespace,expires,digest);`)
	store.Register("capability_headers", 1, `CREATE TABLE capability_headers (namespace TEXT NOT NULL,digest TEXT NOT NULL,shard INTEGER NOT NULL,viewer_id BIGINT NOT NULL,surface TEXT NOT NULL,device_id TEXT NOT NULL,expires BIGINT NOT NULL,PRIMARY KEY(namespace,digest)); CREATE INDEX capability_headers_expiry ON capability_headers(namespace,shard,expires); CREATE INDEX capability_headers_scope ON capability_headers(namespace,shard,viewer_id,surface,expires);`)
}

const ticketNamespace = "ticket"
const ticketShards int64 = HeaderShardCount
const ticketPerShard = 256
const ticketPerScope = 128
const ticketBatchMax = 40
const ticketCleanupMax = 128
const ticketMaintenanceMax = 1024
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
	return store.LockKey(ctx, store.DialectOf(s.db), tx, fmt.Sprintf("capability:admission:ticket:%d", viewer%ticketShards))
}

// IssueBatch authorizes and admits typed tickets atomically.
func (s *Tickets) IssueBatch(ctx context.Context, scope TicketScope, entries []TicketEntry) (int64, error) {
	return s.issueBatch(ctx, scope, entries)
}

func (s *Tickets) issueBatch(ctx context.Context, scope TicketScope, entries []TicketEntry) (int64, error) {
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
	if err = s.lock(ctx, tx, scope.ViewerID); err != nil {
		return 0, err
	}
	now, err := s.now(ctx, tx)
	if err != nil {
		return 0, err
	}
	shard := scope.ViewerID % ticketShards
	// Bounded cleanup of this namespace/shard only. Cascades remove typed rows.
	if _, err = tx.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND expires<=? AND digest IN (SELECT digest FROM capability_headers WHERE namespace=? AND shard=? AND expires<=? ORDER BY expires,digest LIMIT ?)`, ticketNamespace, now, ticketNamespace, shard, now, ticketCleanupMax); err != nil {
		return 0, err
	}
	expiry := now + ticketTTL.Milliseconds()
	var source string
	check := `SELECT g.hash FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.surface=? AND g.viewer_id=? AND g.device_id=? AND g.revoked=0 AND g.expires>? AND u.active=1`
	if store.DialectOf(s.db) == store.Postgres {
		check += ` FOR UPDATE OF g,u`
	}
	if err = tx.QueryRowContext(ctx, check, scope.SourceHash, scope.Surface, scope.ViewerID, scope.DeviceID, now).Scan(&source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, core.ErrUnauthorized
		}
		return 0, err
	}
	headers := make([]Header, 0, len(entries))
	for _, e := range entries {
		headers = append(headers, Header{HeaderScope: HeaderScope{Namespace: ticketNamespace, ViewerID: scope.ViewerID, Surface: scope.Surface, DeviceID: scope.DeviceID}, Digest: e.Digest, ExpiresMillis: expiry})
	}
	if err = AdmitHeadersInTx(ctx, s.db, tx, headers, Limits{PerScope: ticketPerScope, PerShard: ticketPerShard}); err != nil {
		return 0, err
	}
	var child strings.Builder
	child.WriteString(`INSERT INTO capability_tickets(namespace,digest,purpose,source_hash,device) VALUES `)
	args := []any{}
	for i, e := range entries {
		if i > 0 {
			child.WriteString(",")
		}
		child.WriteString("(?,?,?,?,?)")
		args = append(args, ticketNamespace, e.Digest, e.Purpose, scope.SourceHash, scope.DeviceJSON)
	}
	if _, err = tx.ExecContext(ctx, child.String(), args...); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return expiry, nil
}

// Consume uses a guarded DELETE as the linearization point; selection alone
// never authorizes consumption.
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

	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT h.viewer_id,h.device_id,h.expires,t.device,t.source_hash,h.surface,t.purpose FROM capability_headers h LEFT JOIN capability_tickets t ON t.namespace=h.namespace AND t.digest=h.digest WHERE h.namespace=? AND h.digest=?`, ticketNamespace, digest).Scan(&out.ViewerID, &deviceID, &expires, &storedDevice, &storedSource, &storedSurface, &storedPurpose)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, sql.ErrNoRows
	}
	if err != nil {
		return Ticket{}, err
	}
	if !storedPurpose.Valid || !storedDevice.Valid || !storedSource.Valid || storedSurface != surface || storedPurpose.String != purpose {
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
	err = tx.QueryRowContext(ctx, `SELECT h.viewer_id,h.device_id,h.expires,t.device,t.source_hash,h.surface,t.purpose FROM capability_headers h LEFT JOIN capability_tickets t ON t.namespace=h.namespace AND t.digest=h.digest WHERE h.namespace=? AND h.digest=?`, ticketNamespace, digest).Scan(&out.ViewerID, &deviceID, &expires, &storedDevice, &storedSource, &storedSurface, &storedPurpose)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, core.ErrUnauthorized
	}
	if err != nil {
		return Ticket{}, err
	}
	if out.ViewerID != initialViewer || !storedPurpose.Valid || !storedDevice.Valid || !storedSource.Valid || storedSurface != surface || storedPurpose.String != purpose {
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
	var verified string
	check := `SELECT g.hash FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.viewer_id=? AND g.surface=? AND g.device_id=? AND g.revoked=0 AND g.expires>? AND u.active=1`
	if store.DialectOf(s.db) == store.Postgres {
		check += ` FOR UPDATE OF g,u`
	}
	if err = tx.QueryRowContext(ctx, check, out.SourceHash, out.ViewerID, surface, deviceID, now).Scan(&verified); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Ticket{}, core.ErrUnauthorized
		}
		return Ticket{}, err
	}
	consumed, err := ConsumeHeaderInTx(ctx, s.db, tx, Header{HeaderScope: HeaderScope{Namespace: ticketNamespace, ViewerID: out.ViewerID, Surface: surface, DeviceID: deviceID}, Digest: digest}, now)
	if err != nil {
		return Ticket{}, err
	}
	if !consumed {
		return Ticket{}, core.ErrUnauthorized
	}
	if err = tx.Commit(); err != nil {
		return Ticket{}, err
	}
	return out, nil
}

// PruneExpired performs bounded maintenance independently of issuance. Only
// expired rows are deleted; the outer predicate protects concurrent renewal.
// Failure leaves the remaining work for the next pass.
func PruneExpired(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, errors.New("capability: database required")
	}
	ms := now.UnixMilli()
	res, err := db.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND expires<=? AND digest IN (SELECT digest FROM capability_headers WHERE namespace=? AND expires<=? ORDER BY expires,digest LIMIT ?)`, ticketNamespace, ms, ticketNamespace, ms, ticketMaintenanceMax)
	if err != nil {
		return 0, err
	}
	m, err := res.RowsAffected()
	return m, err
}
