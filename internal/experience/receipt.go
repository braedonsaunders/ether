package experience

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("experience_receipts", 1, `
CREATE TABLE experience_receipts (
 hash TEXT PRIMARY KEY, viewer_id INTEGER NOT NULL, device_id TEXT NOT NULL,
 protocol TEXT NOT NULL, expires INTEGER NOT NULL, kind TEXT NOT NULL,
 subject TEXT NOT NULL, purpose TEXT NOT NULL, rank INTEGER NOT NULL
);
CREATE INDEX experience_receipts_expiry ON experience_receipts(expires);
CREATE INDEX experience_receipts_viewer ON experience_receipts(viewer_id, device_id);
CREATE TABLE experience_receipt_actions (
 hash TEXT NOT NULL REFERENCES experience_receipts(hash) ON DELETE CASCADE,
 action TEXT NOT NULL, PRIMARY KEY(hash, action)
);`)
}

func init() {
	store.Register("experience_receipts", 2, `ALTER TABLE experience_receipts ADD COLUMN impression TEXT NOT NULL DEFAULT '';`)
}

func init() {
	store.Register("experience_receipts", 3, `ALTER TABLE experience_receipts ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;`)
}

func init() {
	store.Register("experience_receipts", 4, `CREATE INDEX experience_receipts_scope_expiry ON experience_receipts(viewer_id,device_id,expires,hash);`)
}

// ReceiptSubject is server-owned neutral information, never supplied by a client report.
type ReceiptSubject struct {
	Kind, ID   string
	Purpose    core.RowPurpose
	Rank       int
	Impression *core.NeutralImpression
}

type ReceiptOptions struct {
	DB            *sql.DB
	Active        func(context.Context, int64) (*core.User, error)
	Allowed       func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject) bool
	AllowedAction func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) bool
	Record        func(context.Context, *core.User, core.Device, core.Protocol, ReceiptSubject, frontend.RowInteractionKind) error
	Now           func() time.Time
	TTL           time.Duration
	MaxPerDevice  int
	// MirrorHeaders mirrors digests into capability headers inside the legacy
	// transaction. Off leaves experience_receipts as the only store. Headers
	// are bookkeeping: report, revocation, and quota never consult them.
	MirrorHeaders bool
	// Generation, when set, is stored on the authority row and rechecked on
	// report. Nil stores 0 and does not recheck, which is the mixed-version
	// behavior for callers that have no generation.
	Generation func(context.Context, *core.User, core.Device, core.Protocol) (int64, error)
	// Test seams. Nil uses capability.BindHeadersInTx and DropHeadersInTx.
	bindHeaders func(context.Context, *sql.Tx, []capability.Header) error
	beforeSweep func(*sql.Tx)
	dropHeaders func(context.Context, *sql.Tx, capability.HeaderScope, []string) error
}

type Receipts struct {
	opts  ReceiptOptions
	codec *handles.Codec
}

func NewReceipts(o ReceiptOptions) (*Receipts, error) {
	if o.DB == nil || o.Active == nil || o.Allowed == nil || o.AllowedAction == nil || o.Record == nil || o.TTL < time.Second || o.MaxPerDevice <= 0 {
		return nil, errors.New("experience: incomplete receipt dependencies")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	// HashRaw preserves the existing receipt digest: sha256 of the 32 token
	// bytes, not the wire encoding. The wire form stays unprefixed base64.
	codec, err := handles.NewCodec(rand.Reader, "", handles.HashRaw)
	if err != nil {
		return nil, err
	}
	return &Receipts{opts: o, codec: codec}, nil
}

var ErrReceipt = errors.New("experience: invalid interaction receipt")

func snapshotMatches(subject ReceiptSubject, imp *core.NeutralImpression) bool {
	if imp == nil || imp.RowID == "" || imp.Purpose != subject.Purpose || imp.Rank != subject.Rank {
		return false
	}
	if subject.Kind == "row" {
		return imp.RowID == subject.ID
	}
	for _, entry := range imp.Entries {
		if entry.Kind == subject.Kind && entry.ID != "" && subject.ID == imp.RowID+"/"+entry.Kind+"/"+entry.ID {
			return true
		}
	}
	return false
}

// Issue creates an opaque, surface-scoped receipt. A device identifier is required.
func (s *Receipts) Issue(ctx context.Context, viewer *core.User, device core.Device, protocol core.Protocol, subject ReceiptSubject) (string, error) {
	if viewer == nil || viewer.ID <= 0 || device.ID == "" || protocol == "" || subject.Kind == "" || subject.ID == "" {
		return "", ErrReceipt
	}
	var snapshot string
	if subject.Impression != nil {
		imp := subject.Impression
		if !snapshotMatches(subject, imp) || len(imp.Items) > 1000 || len(imp.Entries) > 1000 || len(imp.Placement) > 128 || len(imp.ViewID) > 512 || len(imp.RowID) > 512 || len(imp.Kind) > 512 || len(imp.Title) > 2048 {
			return "", ErrReceipt
		}
		b, err := json.Marshal(imp)
		if err != nil || len(b) > 32768 {
			return "", ErrReceipt
		}
		snapshot = string(b)
	}
	active, err := s.opts.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID || !s.opts.Allowed(ctx, active, device, protocol, subject) {
		return "", ErrReceipt
	}
	var generation int64
	if s.opts.Generation != nil {
		generation, err = s.opts.Generation(ctx, active, device, protocol)
		if err != nil || generation < 0 {
			return "", ErrReceipt
		}
	}
	tx, err := s.opts.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// This is the only lock on the issue path. Header bind and drop run after
	// it, on this same transaction, and do not take a lock of their own.
	if err = store.LockKey(ctx, store.DialectOf(s.opts.DB), tx, fmt.Sprintf("experience:receipts:%d:%s", viewer.ID, device.ID)); err != nil {
		return "", err
	}
	now := s.opts.Now().Unix()
	if err = s.sweepExpired(ctx, tx, viewer.ID, device.ID, now); err != nil {
		return "", err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=? AND device_id=? AND expires>?`, viewer.ID, device.ID, now).Scan(&count); err != nil {
		return "", err
	}
	if count >= s.opts.MaxPerDevice {
		return "", errors.New("experience: receipt limit reached")
	}
	for i := 0; i < 3; i++ {
		token, hash, err := s.codec.Issue()
		if err != nil {
			return "", err
		}
		expires := s.opts.Now().Add(s.opts.TTL).Unix()
		res, e := tx.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank,impression,generation) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO NOTHING`, hash, viewer.ID, device.ID, string(protocol), expires, subject.Kind, subject.ID, string(subject.Purpose), subject.Rank, snapshot, generation)
		if e != nil {
			return "", e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return "", e
		}
		if n == 0 {
			continue
		}
		// A bind failure aborts the transaction. Do not retry inside it.
		if err = s.mirrorIssued(ctx, tx, viewer.ID, device.ID, string(protocol), hash, expires); err != nil {
			return "", err
		}
		if err = tx.Commit(); err != nil {
			return "", err
		}
		return token, nil
	}
	return "", errors.New("experience: receipt collision")
}

// Report rechecks membership and authorization. Duplicate actions are ignored
// across nodes. Recording is at-most-once: a sink failure is not retried.
func (s *Receipts) Report(ctx context.Context, interaction frontend.RowInteraction) error {
	if interaction.Viewer == nil || interaction.Device.ID == "" || interaction.Protocol == "" {
		return ErrReceipt
	}
	switch interaction.Kind {
	case frontend.RowImpression, frontend.RowClick, frontend.RowPlay:
	default:
		return ErrReceipt
	}
	hash, ok := s.codec.Hash(interaction.Handle)
	if !ok {
		return ErrReceipt
	}
	var uid, expires, generation int64
	var device, protocol, kind, id, purpose, snapshot string
	var rank int
	err := s.opts.DB.QueryRowContext(ctx, `SELECT viewer_id,device_id,protocol,expires,kind,subject,purpose,rank,impression,generation FROM experience_receipts WHERE hash=?`, hash).Scan(&uid, &device, &protocol, &expires, &kind, &id, &purpose, &rank, &snapshot, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReceipt
	}
	if err != nil {
		return err
	}
	if uid != interaction.Viewer.ID || device != interaction.Device.ID || protocol != string(interaction.Protocol) || expires <= s.opts.Now().Unix() {
		return ErrReceipt
	}
	viewer, err := s.opts.Active(ctx, uid)
	subject := ReceiptSubject{Kind: kind, ID: id, Purpose: core.RowPurpose(purpose), Rank: rank}
	if snapshot != "" {
		if len(snapshot) > 32768 {
			return ErrReceipt
		}
		var imp core.NeutralImpression
		if json.Unmarshal([]byte(snapshot), &imp) != nil || !snapshotMatches(subject, &imp) || len(imp.Items) > 1000 || len(imp.Entries) > 1000 {
			return ErrReceipt
		}
		subject.Impression = &imp
	}
	if err != nil || viewer == nil || viewer.ID != uid || !s.opts.Allowed(ctx, viewer, interaction.Device, interaction.Protocol, subject) {
		return ErrReceipt
	}
	if !s.opts.AllowedAction(ctx, viewer, interaction.Device, interaction.Protocol, subject, interaction.Kind) {
		return ErrReceipt
	}
	if s.opts.Generation != nil {
		current, gerr := s.opts.Generation(ctx, viewer, interaction.Device, interaction.Protocol)
		if gerr != nil || current < 0 || current != generation {
			return ErrReceipt
		}
	}
	// Policy and membership callbacks may use this same database; do not hold
	// a connection/transaction while invoking them. The legacy row is the only
	// authority. Capability headers are not read here, so a header without a
	// legacy row cannot authorize and a legacy row does not fall back to one.
	tx, err := s.opts.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.opts.DB), tx, "experience:receipt:"+hash); err != nil {
		return err
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM experience_receipts WHERE hash=? AND viewer_id=? AND device_id=? AND protocol=? AND expires>? AND generation=?`, hash, uid, device, protocol, s.opts.Now().Unix(), generation).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReceipt
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO experience_receipt_actions(hash,action) VALUES(?,?) ON CONFLICT(hash,action) DO NOTHING`, hash, string(interaction.Kind))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	return s.opts.Record(ctx, viewer, interaction.Device, interaction.Protocol, subject, interaction.Kind)
}

var _ frontend.RowInteractionReporter = (*Receipts)(nil)

func (s *Receipts) ReportRowInteraction(ctx context.Context, i frontend.RowInteraction) error {
	return s.Report(ctx, i)
}
