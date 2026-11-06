package clusterconfig

import (
	"context"
	"database/sql"
	"errors"

	"github.com/braedonsaunders/ether/internal/store"
)

// NodeControlRecord contains only the typed, nonsecret nodecontrol document.
// Persistence belongs here; app wiring must not query these tables.
type NodeControlRecord struct {
	Version  int64
	Document []byte
}

var ErrNodeControlConflict = errors.New("node control conflict")

func init() {
	store.Register("clusterconfig", 2, `
CREATE TABLE clusterconfig_node_control(node_id TEXT NOT NULL, boot TEXT NOT NULL, version INTEGER NOT NULL, document TEXT NOT NULL, PRIMARY KEY(node_id,boot));
CREATE TABLE clusterconfig_node_requests(node_id TEXT NOT NULL, boot TEXT NOT NULL, request_id TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(node_id,boot,request_id));
`)
}

type NodeControls struct{ db *sql.DB }

func NewNodeControls(db *sql.DB) *NodeControls { return &NodeControls{db: db} }
func (s *NodeControls) Load(ctx context.Context, node, boot string) (NodeControlRecord, error) {
	var r NodeControlRecord
	err := s.db.QueryRowContext(ctx, `SELECT version,document FROM clusterconfig_node_control WHERE node_id=? AND boot=?`, node, boot).Scan(&r.Version, &r.Document)
	return r, err
}
func (s *NodeControls) Initialize(ctx context.Context, node, boot string, doc []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO clusterconfig_node_control(node_id,boot,version,document) VALUES(?,?,0,?) ON CONFLICT(node_id,boot) DO NOTHING`, node, boot, string(doc))
	return err
}

// CompareAndSwap orders requester writes and target observations. A DB revision
// is independent of the user-visible requested/applied command versions.
func (s *NodeControls) CompareAndSwap(ctx context.Context, node, boot string, version int64, doc []byte) error {
	res, err := s.db.ExecContext(ctx, `UPDATE clusterconfig_node_control SET version=version+1,document=? WHERE node_id=? AND boot=? AND version=?`, string(doc), node, boot, version)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrNodeControlConflict
	}
	return err
}

// Submit atomically deduplicates a request and replaces the current document.
// Reusing an ID for a different payload always fails, including after completion.
func (s *NodeControls) Submit(ctx context.Context, node, boot, id string, payload, doc []byte, version int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO clusterconfig_node_requests(node_id,boot,request_id,payload) VALUES(?,?,?,?) ON CONFLICT(node_id,boot,request_id) DO NOTHING`, node, boot, id, string(payload))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var old string
		err = tx.QueryRowContext(ctx, `SELECT payload FROM clusterconfig_node_requests WHERE node_id=? AND boot=? AND request_id=?`, node, boot, id).Scan(&old)
		if err != nil {
			return false, err
		}
		if old != string(payload) {
			return false, ErrNodeControlConflict
		}
		return true, nil
	}
	res, err = tx.ExecContext(ctx, `UPDATE clusterconfig_node_control SET version=version+1,document=? WHERE node_id=? AND boot=? AND version=?`, string(doc), node, boot, version)
	if err != nil {
		return false, err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, ErrNodeControlConflict
	}
	return false, tx.Commit()
}

// RequestMatches permits retrying a completed request without running it again.
func (s *NodeControls) RequestMatches(ctx context.Context, node, boot, id string, payload []byte) (bool, error) {
	var old string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM clusterconfig_node_requests WHERE node_id=? AND boot=? AND request_id=?`, node, boot, id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if old != string(payload) {
		return false, ErrNodeControlConflict
	}
	return true, nil
}
