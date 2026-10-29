package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("frontend_tickets", "auth", "identity_grants")
	store.Register("frontend_tickets", 1, `CREATE TABLE frontend_tickets (hash TEXT PRIMARY KEY, surface TEXT NOT NULL, purpose TEXT NOT NULL, viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, source_hash TEXT NOT NULL REFERENCES identity_grants(hash) ON DELETE CASCADE, device TEXT NOT NULL, expires INTEGER NOT NULL); CREATE INDEX frontend_tickets_expiry ON frontend_tickets(expires);`)
}

type TicketService struct {
	db      *sql.DB
	surface frontend.Protocol
	grants  *Grants
	now     func() time.Time
}

func NewTicketService(db *sql.DB, surface frontend.Protocol, grants *Grants, now func() time.Time) (*TicketService, error) {
	if db == nil || surface == "" || grants == nil {
		return nil, errors.New("tickets: missing dependency")
	}
	if now == nil {
		now = time.Now
	}
	return &TicketService{db, surface, grants, now}, nil
}
func ticketHash(token string) (string, bool) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(token)
	if e != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != token {
		return "", false
	}
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:]), true
}
func (s *TicketService) Issue(ctx context.Context, credential string, device frontend.Device, purpose string) (string, error) {
	if purpose == "" || len(purpose) > 64 || device.ID == "" {
		return "", core.ErrUnauthorized
	}
	v, err := s.grants.Resolve(ctx, string(s.surface), credential)
	if err != nil || v == nil {
		return "", core.ErrUnauthorized
	}
	source, ok := grantHash(credential)
	if !ok {
		return "", core.ErrUnauthorized
	}
	data, err := json.Marshal(device)
	if err != nil || len(data) > 4096 {
		return "", core.ErrUnauthorized
	}
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	hash, _ := ticketHash(token)
	now := s.now().UnixMilli()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM frontend_tickets WHERE expires<=?`, now)
	res, err := s.db.ExecContext(ctx, `INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) SELECT ?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.surface=? AND g.viewer_id=? AND g.device_id=? AND g.revoked=0 AND g.expires>? AND u.active=1)`, hash, string(s.surface), purpose, v.ID, source, string(data), now+30000, source, string(s.surface), v.ID, device.ID, now)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", core.ErrUnauthorized
	}
	return token, nil
}
func (s *TicketService) Consume(ctx context.Context, token, purpose string) (frontend.Ticket, error) {
	var out frontend.Ticket
	hash, ok := ticketHash(token)
	if !ok || purpose == "" {
		return out, core.ErrUnauthorized
	}
	now := s.now().UnixMilli()
	var viewerID int64
	var data string
	err := s.db.QueryRowContext(ctx, `DELETE FROM frontend_tickets WHERE hash=? AND surface=? AND purpose=? AND expires>? AND EXISTS(SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=frontend_tickets.source_hash AND g.viewer_id=frontend_tickets.viewer_id AND g.surface=frontend_tickets.surface AND g.revoked=0 AND g.expires>? AND u.active=1) RETURNING viewer_id,device`, hash, string(s.surface), purpose, now, now).Scan(&viewerID, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return out, core.ErrUnauthorized
	}
	if err != nil {
		return out, err
	}
	v, err := s.grants.viewers.ActiveUser(ctx, viewerID)
	if err != nil || v == nil {
		return out, core.ErrUnauthorized
	}
	if err = json.Unmarshal([]byte(data), &out.Device); err != nil {
		return frontend.Ticket{}, err
	}
	out.Viewer = v
	return out, nil
}
