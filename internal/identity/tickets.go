package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/handles"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.RegisterAfter("frontend_tickets", "auth", "identity_grants")
	store.Register("frontend_tickets", 2, `CREATE INDEX frontend_tickets_scope_expiry ON frontend_tickets(viewer_id,surface,expires,hash);`)
	store.Register("frontend_tickets", 1, `CREATE TABLE frontend_tickets (hash TEXT PRIMARY KEY, surface TEXT NOT NULL, purpose TEXT NOT NULL, viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, source_hash TEXT NOT NULL REFERENCES identity_grants(hash) ON DELETE CASCADE, device TEXT NOT NULL, expires INTEGER NOT NULL); CREATE INDEX frontend_tickets_expiry ON frontend_tickets(expires);`)
}

// The legacy table remains the single consume authority during rolling
// upgrades. Old consumers may leave shared headers charged until expiry and
// old issuers bypass the new quota. Retiring the legacy table requires an
// independently verified all-writer compatibility fence and migration.
type ticketEngine interface {
	IssueMirrored(context.Context, capability.TicketScope, []capability.TicketEntry) (int64, error)
	ConsumeMirrored(context.Context, string, string, string) (capability.Ticket, error)
}

type TicketService struct {
	db      *sql.DB
	surface frontend.Protocol
	grants  *Grants
	now     func() time.Time
	codec   *handles.Codec
	engine  ticketEngine
}

func NewTicketService(db *sql.DB, surface frontend.Protocol, grants *Grants, now func() time.Time) (*TicketService, error) {
	if db == nil || surface == "" || grants == nil {
		return nil, errors.New("tickets: missing dependency")
	}
	if now == nil {
		now = time.Now
	}
	codec, err := handles.NewCodec(rand.Reader, "", handles.HashWire)
	if err != nil {
		return nil, err
	}
	engine, err := capability.NewTicketsWithClock(db, now)
	if err != nil {
		return nil, err
	}
	return &TicketService{db: db, surface: surface, grants: grants, now: now, codec: codec, engine: engine}, nil
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
	token, hash, err := s.codec.Issue()
	if err != nil {
		return "", err
	}
	_, err = s.engine.IssueMirrored(ctx, capability.TicketScope{ViewerID: v.ID, Surface: string(s.surface), DeviceID: device.ID, SourceHash: source, DeviceJSON: string(data)}, []capability.TicketEntry{{Digest: hash, Purpose: purpose}})
	if err != nil {
		return "", err
	}
	return token, nil
}
func (s *TicketService) Consume(ctx context.Context, token, purpose string) (frontend.Ticket, error) {
	var out frontend.Ticket
	hash, ok := s.codec.Hash(token)
	if !ok || purpose == "" {
		return out, core.ErrUnauthorized
	}
	result, err := s.engine.ConsumeMirrored(ctx, string(s.surface), purpose, hash)
	if err != nil {
		return out, err
	}
	viewerID, data := result.ViewerID, result.DeviceJSON
	out.SourceHash = result.SourceHash
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

// Active rechecks the issuing grant and membership for a live socket.
func (s *TicketService) Active(ctx context.Context, t frontend.Ticket) (bool, error) {
	if t.Viewer == nil || t.Viewer.ID <= 0 || t.SourceHash == "" || t.Device.ID == "" {
		return false, core.ErrUnauthorized
	}
	var valid bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.surface=? AND g.viewer_id=? AND g.device_id=? AND g.revoked=0 AND g.expires>? AND u.active=1)`, t.SourceHash, string(s.surface), t.Viewer.ID, t.Device.ID, s.now().UnixMilli()).Scan(&valid)
	return valid, err
}
