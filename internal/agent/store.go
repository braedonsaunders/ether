// Package agent is Ask Ether: a bounded tool-calling loop over the shared
// tool catalogue, streaming the AI SDK UI message stream, with durable
// transcripts, approvals, secret requests and a tool-call audit.
package agent

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("agent", 1, `
CREATE TABLE agent_threads (
  id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE agent_messages (
  id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, seq INTEGER NOT NULL,
  role TEXT NOT NULL, parts TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX agent_messages_thread ON agent_messages(thread_id, seq);
CREATE TABLE agent_approvals (
  id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, message_id TEXT NOT NULL,
  tool_name TEXT NOT NULL, input TEXT NOT NULL, category TEXT NOT NULL,
  description TEXT NOT NULL, details TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'pending', decision_note TEXT NOT NULL DEFAULT '',
  failure_reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
  decided_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX agent_approvals_thread ON agent_approvals(thread_id, status);
CREATE TABLE agent_secret_requests (
  id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, message_id TEXT NOT NULL,
  provider_label TEXT NOT NULL, credential_label TEXT NOT NULL,
  purpose TEXT NOT NULL, help_url TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending', created_at INTEGER NOT NULL);
CREATE INDEX agent_secret_requests_thread ON agent_secret_requests(thread_id, status);
CREATE TABLE agent_audit (
  id TEXT PRIMARY KEY, thread_id TEXT NOT NULL DEFAULT '', tool_name TEXT NOT NULL,
  principal TEXT NOT NULL, duration_ms INTEGER NOT NULL, status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE INDEX agent_audit_created ON agent_audit(created_at);
`)
}

func newID(prefix string) string {
	return prefix + rand.Text()
}

// Thread is one conversation.
type Thread struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"createdAt"` // unix milliseconds
	UpdatedAt int64  `json:"updatedAt"`
}

// Message is one stored UI message.
type Message struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Parts     json.RawMessage `json:"parts"`
	CreatedAt string          `json:"createdAt,omitempty"` // RFC3339, for AgentPanel
	seq       int64
}

// Approval is one durable proposal awaiting (or past) a decision.
type Approval struct {
	ID, ThreadID, MessageID, ToolName string
	Input                             map[string]any
	Category, Description             string
	Details                           json.RawMessage
	Status, DecisionNote              string
	FailureReason                     string
	// Result is returned to the editor after approval; it is not persisted.
	Result map[string]any
}

// SecretReq is one durable credential request (never the value).
type SecretReq struct {
	ID, ThreadID, MessageID string
	ProviderLabel           string
	CredentialLabel         string
	Purpose, HelpURL        string
	Status                  string
}

// Store persists threads, messages, approvals, secret requests and audit.
type Store struct{ db *sql.DB }

// NewStore builds a Store.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func nowMs() int64 { return time.Now().UnixMilli() }

func (s *Store) CreateThread(ctx context.Context, title string) (Thread, error) {
	th := Thread{ID: newID("thr_"), Title: title, CreatedAt: nowMs(), UpdatedAt: nowMs()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_threads(id,title,created_at,updated_at) VALUES(?,?,?,?)`,
		th.ID, th.Title, th.CreatedAt, th.UpdatedAt)
	return th, err
}

func (s *Store) Thread(ctx context.Context, id string) (Thread, bool, error) {
	var th Thread
	err := s.db.QueryRowContext(ctx, `SELECT id,title,created_at,updated_at FROM agent_threads WHERE id=?`, id).
		Scan(&th.ID, &th.Title, &th.CreatedAt, &th.UpdatedAt)
	if err == sql.ErrNoRows {
		return th, false, nil
	}
	return th, err == nil, err
}

func (s *Store) ListThreads(ctx context.Context, limit int) ([]Thread, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,title,created_at,updated_at FROM agent_threads ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Thread{}
	for rows.Next() {
		var th Thread
		if err := rows.Scan(&th.ID, &th.Title, &th.CreatedAt, &th.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, th)
	}
	return out, rows.Err()
}

func (s *Store) DeleteThread(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM agent_messages WHERE thread_id=?`,
		`DELETE FROM agent_approvals WHERE thread_id=?`,
		`DELETE FROM agent_secret_requests WHERE thread_id=?`,
		`DELETE FROM agent_threads WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) touchThread(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE agent_threads SET updated_at=? WHERE id=?`, nowMs(), id)
}

// AppendMessage appends a message and returns it with its sequence.
func (s *Store) AppendMessage(ctx context.Context, threadID, role string, parts json.RawMessage) (Message, error) {
	m := Message{ID: newID("msg_"), Role: role, Parts: parts}
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM agent_messages WHERE thread_id=?`, threadID).Scan(&seq); err != nil {
		return m, err
	}
	m.seq = seq.Int64 + 1
	ms := nowMs()
	m.CreatedAt = time.UnixMilli(ms).UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_messages(id,thread_id,seq,role,parts,created_at) VALUES(?,?,?,?,?,?)`,
		m.ID, threadID, m.seq, role, string(parts), ms); err != nil {
		return m, err
	}
	s.touchThread(ctx, threadID)
	return m, nil
}

// UpdateParts replaces a message's parts (streaming assistant message).
func (s *Store) UpdateParts(ctx context.Context, messageID string, parts json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_messages SET parts=? WHERE id=?`, string(parts), messageID)
	return err
}

// Messages returns up to limit messages of a thread, oldest first. When
// before names a message id, only messages before it are considered (the
// latest page ending before that message).
func (s *Store) Messages(ctx context.Context, threadID, before string, limit int) ([]Message, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	beforeSeq := int64(1) << 62
	if before != "" {
		var seq sql.NullInt64
		err := s.db.QueryRowContext(ctx, `SELECT seq FROM agent_messages WHERE thread_id=? AND id=?`, threadID, before).Scan(&seq)
		if err != nil && err != sql.ErrNoRows {
			return nil, false, err
		}
		if seq.Valid {
			beforeSeq = seq.Int64
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,role,parts,created_at,seq FROM agent_messages WHERE thread_id=? AND seq<? ORDER BY seq DESC LIMIT ?`,
		threadID, beforeSeq, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var rev []Message
	for rows.Next() {
		var m Message
		var parts string
		var ms int64
		if err := rows.Scan(&m.ID, &m.Role, &parts, &ms, &m.seq); err != nil {
			return nil, false, err
		}
		m.Parts = json.RawMessage(parts)
		m.CreatedAt = time.UnixMilli(ms).UTC().Format(time.RFC3339)
		rev = append(rev, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasOlder := len(rev) > limit
	if hasOlder {
		rev = rev[:limit]
	}
	out := make([]Message, len(rev))
	for i, m := range rev {
		out[len(rev)-1-i] = m
	}
	return out, hasOlder, nil
}

// AllMessages returns every message of a thread (loop history; capped by caller).
func (s *Store) AllMessages(ctx context.Context, threadID string) ([]Message, error) {
	msgs, _, err := s.Messages(ctx, threadID, "", 200)
	return msgs, err
}

func (s *Store) CreateApproval(ctx context.Context, a Approval) (Approval, error) {
	a.ID = newID("apr_")
	if a.Status == "" {
		a.Status = "pending"
	}
	input, _ := json.Marshal(a.Input)
	details := a.Details
	if len(details) == 0 {
		details = json.RawMessage(`[]`)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_approvals(id,thread_id,message_id,tool_name,input,category,description,details,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ThreadID, a.MessageID, a.ToolName, string(input), a.Category, a.Description, string(details), a.Status, nowMs())
	return a, err
}

func (s *Store) GetApproval(ctx context.Context, id string) (Approval, bool, error) {
	var a Approval
	var input, details string
	err := s.db.QueryRowContext(ctx, `SELECT id,thread_id,message_id,tool_name,input,category,description,details,status,decision_note,failure_reason FROM agent_approvals WHERE id=?`, id).
		Scan(&a.ID, &a.ThreadID, &a.MessageID, &a.ToolName, &input, &a.Category, &a.Description, &details, &a.Status, &a.DecisionNote, &a.FailureReason)
	if err == sql.ErrNoRows {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	_ = json.Unmarshal([]byte(input), &a.Input)
	a.Details = json.RawMessage(details)
	return a, true, nil
}

// ClaimApproval atomically prevents multiple decisions or applications, even
// when requests arrive at different cluster nodes.
func (s *Store) ClaimApproval(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE agent_approvals SET status='applying' WHERE id=? AND status='pending'`, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) UpdateApproval(ctx context.Context, a Approval) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_approvals SET status=?,decision_note=?,failure_reason=?,decided_at=? WHERE id=?`,
		a.Status, a.DecisionNote, a.FailureReason, nowMs(), a.ID)
	return err
}

func (s *Store) claimSecret(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE agent_secret_requests SET status='storing' WHERE id=? AND status='pending'`, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) CreateSecret(ctx context.Context, r SecretReq) (SecretReq, error) {
	r.ID = newID("sec_")
	r.Status = "pending"
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_secret_requests(id,thread_id,message_id,provider_label,credential_label,purpose,help_url,status,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, r.ThreadID, r.MessageID, r.ProviderLabel, r.CredentialLabel, r.Purpose, r.HelpURL, r.Status, nowMs())
	return r, err
}

func (s *Store) GetSecret(ctx context.Context, id string) (SecretReq, bool, error) {
	var r SecretReq
	err := s.db.QueryRowContext(ctx, `SELECT id,thread_id,message_id,provider_label,credential_label,purpose,help_url,status FROM agent_secret_requests WHERE id=?`, id).
		Scan(&r.ID, &r.ThreadID, &r.MessageID, &r.ProviderLabel, &r.CredentialLabel, &r.Purpose, &r.HelpURL, &r.Status)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

func (s *Store) UpdateSecretStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_secret_requests SET status=? WHERE id=?`, status, id)
	return err
}

// Audit records one tool call. Inputs are never stored.
func (s *Store) Audit(ctx context.Context, threadID, tool, principal, status, mappedErr string, dur time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_audit(id,thread_id,tool_name,principal,duration_ms,status,error,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		newID("aud_"), threadID, tool, principal, dur.Milliseconds(), status, mappedErr, nowMs())
	if err != nil {
		return fmt.Errorf("agent audit: %w", err)
	}
	return nil
}

// AuditRetention is how long a tool-call audit row is kept.
const AuditRetention = 90 * 24 * time.Hour

// Prune deletes audit rows older than AuditRetention through
// agent_audit_created. Nothing reads the audit back, so this only bounds
// its growth.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-AuditRetention).UnixMilli()
	return store.PruneBatched(ctx, s.db,
		`DELETE FROM agent_audit WHERE id IN (SELECT id FROM agent_audit WHERE created_at<? ORDER BY created_at LIMIT ?)`, cutoff)
}
