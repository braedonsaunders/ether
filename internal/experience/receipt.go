package experience

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
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
}

type Receipts struct{ opts ReceiptOptions }

func NewReceipts(o ReceiptOptions) (*Receipts, error) {
	if o.DB == nil || o.Active == nil || o.Allowed == nil || o.AllowedAction == nil || o.Record == nil || o.TTL < time.Second || o.MaxPerDevice <= 0 {
		return nil, errors.New("experience: incomplete receipt dependencies")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Receipts{opts: o}, nil
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
	tx, err := s.opts.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.opts.DB), tx, fmt.Sprintf("experience:receipts:%d:%s", viewer.ID, device.ID)); err != nil {
		return "", err
	}
	now := s.opts.Now().Unix()
	if _, err = tx.ExecContext(ctx, `DELETE FROM experience_receipts WHERE expires<=?`, now); err != nil {
		return "", err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts WHERE viewer_id=? AND device_id=?`, viewer.ID, device.ID).Scan(&count); err != nil {
		return "", err
	}
	if count >= s.opts.MaxPerDevice {
		return "", errors.New("experience: receipt limit reached")
	}
	for i := 0; i < 3; i++ {
		raw := make([]byte, 32)
		if _, err = rand.Read(raw); err != nil {
			return "", err
		}
		handle := base64.RawURLEncoding.EncodeToString(raw)
		sum := sha256.Sum256(raw)
		res, e := tx.ExecContext(ctx, `INSERT INTO experience_receipts(hash,viewer_id,device_id,protocol,expires,kind,subject,purpose,rank,impression) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hash) DO NOTHING`, hex.EncodeToString(sum[:]), viewer.ID, device.ID, string(protocol), s.opts.Now().Add(s.opts.TTL).Unix(), subject.Kind, subject.ID, string(subject.Purpose), subject.Rank, snapshot)
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
		if err = tx.Commit(); err != nil {
			return "", err
		}
		return handle, nil
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
	raw, err := base64.RawURLEncoding.DecodeString(interaction.Handle)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != interaction.Handle {
		return ErrReceipt
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	var uid, expires int64
	var device, protocol, kind, id, purpose, snapshot string
	var rank int
	err = s.opts.DB.QueryRowContext(ctx, `SELECT viewer_id,device_id,protocol,expires,kind,subject,purpose,rank,impression FROM experience_receipts WHERE hash=?`, hash).Scan(&uid, &device, &protocol, &expires, &kind, &id, &purpose, &rank, &snapshot)
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
	// Policy and membership callbacks may use this same database; do not hold
	// a connection/transaction while invoking them.
	tx, err := s.opts.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.opts.DB), tx, "experience:receipt:"+hash); err != nil {
		return err
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM experience_receipts WHERE hash=? AND viewer_id=? AND device_id=? AND protocol=? AND expires>?`, hash, uid, device, protocol, s.opts.Now().Unix()).Scan(&exists)
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
