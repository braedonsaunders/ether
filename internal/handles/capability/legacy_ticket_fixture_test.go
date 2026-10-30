// Test-only legacy fixture copied from b4ef791d53a68741c0f1513071328594b62eb9ed internal/identity/tickets.go and grants.go.
// Adaptations: exported grant/viewer dependencies, package and symbol names; no migrations or Active method.
package capability_test

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
	"github.com/braedonsaunders/ether/internal/identity"
	"strings"
)

type legacyTicketService struct {
	db      *sql.DB
	surface frontend.Protocol
	grants  *identity.Grants
	now     func() time.Time
	viewers identity.ActiveViewer
}

func newLegacyTicketService(db *sql.DB, surface frontend.Protocol, grants *identity.Grants, now func() time.Time) (*legacyTicketService, error) {
	if db == nil || surface == "" || grants == nil {
		return nil, errors.New("tickets: missing dependency")
	}
	if now == nil {
		now = time.Now
	}
	return &legacyTicketService{db, surface, grants, now, activeFixture{}}, nil
}
func ticketHash(token string) (string, bool) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(token)
	if e != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != token {
		return "", false
	}
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:]), true
}
func (s *legacyTicketService) Issue(ctx context.Context, credential string, device frontend.Device, purpose string) (string, error) {
	if purpose == "" || len(purpose) > 64 || device.ID == "" {
		return "", core.ErrUnauthorized
	}
	v, err := s.grants.Resolve(ctx, string(s.surface), credential)
	if err != nil || v == nil {
		return "", core.ErrUnauthorized
	}
	source, ok := legacyGrantHash(credential)
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
func (s *legacyTicketService) Consume(ctx context.Context, token, purpose string) (frontend.Ticket, error) {
	var out frontend.Ticket
	hash, ok := ticketHash(token)
	if !ok || purpose == "" {
		return out, core.ErrUnauthorized
	}
	now := s.now().UnixMilli()
	var viewerID int64
	var data string
	err := s.db.QueryRowContext(ctx, `DELETE FROM frontend_tickets WHERE hash=? AND surface=? AND purpose=? AND expires>? AND EXISTS(SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=frontend_tickets.source_hash AND g.viewer_id=frontend_tickets.viewer_id AND g.surface=frontend_tickets.surface AND g.revoked=0 AND g.expires>? AND u.active=1) RETURNING viewer_id,device,source_hash`, hash, string(s.surface), purpose, now, now).Scan(&viewerID, &data, &out.SourceHash)
	if errors.Is(err, sql.ErrNoRows) {
		return out, core.ErrUnauthorized
	}
	if err != nil {
		return out, err
	}
	v, err := s.viewers.ActiveUser(ctx, viewerID)
	if err != nil || v == nil {
		return out, core.ErrUnauthorized
	}
	if err = json.Unmarshal([]byte(data), &out.Device); err != nil {
		return frontend.Ticket{}, err
	}
	out.Viewer = v
	return out, nil
}

func legacyGrantHash(credential string) (string, bool) {
	if !identity.IsGrantCredential(credential) {
		return "", false
	}
	encoded := strings.TrimPrefix(credential, core.GrantCredentialPrefix)
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != encoded {
		return "", false
	}
	h := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(h[:]), true
}
