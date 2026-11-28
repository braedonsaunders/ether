package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/braedonsaunders/ether/internal/httpjson"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
)

// PairingOptions configures the pairing service.
type PairingOptions struct {
	DB     *sql.DB
	Grants *identity.Grants
	// Authorize returns the owner viewer id for a console request. The app
	// implements it from the admin API's owner check.
	Authorize    func(ctx context.Context) (int64, error)
	ClientIP     func(*http.Request) (netip.Addr, bool)
	Logger       *slog.Logger
	AllowedHosts []string
	Now          func() time.Time
}

// Pairing serves the owner console and OAuth routes.
type Pairing struct {
	opts PairingOptions
	log  *slog.Logger

	mu       sync.Mutex
	attempts map[string][]time.Time // attempts per remote address
}

// NewPairing builds the pairing service.
func NewPairing(opts PairingOptions) (*Pairing, error) {
	if opts.DB == nil || opts.Grants == nil || opts.Authorize == nil {
		return nil, fmt.Errorf("mcp: pairing needs a database, grants and authorize")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Pairing{opts: opts, log: log, attempts: map[string][]time.Time{}}, nil
}

func (p *Pairing) now() time.Time {
	if p.opts.Now != nil {
		return p.opts.Now()
	}
	return time.Now()
}

// Handler serves the console routes, mounted by the app under
// /api/agent/mcp (owner-authenticated by the admin API):
//
//	GET    /authorization   -> pending request details
//	POST   /authorization   -> approve or deny
//	GET    /clients         -> {clients: [...]}
//	DELETE /clients/{id}    -> 204
func (p *Pairing) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorization", p.authorizationDetails)
	mux.HandleFunc("POST /authorization", p.authorizationDecision)
	mux.HandleFunc("GET /clients", p.handleClients)
	mux.HandleFunc("DELETE /clients/{id}", p.handleRevoke)
	return mux
}

// Authenticate accepts only scoped grant credentials, never pairing codes.
func (p *Pairing) Authenticate(ctx context.Context, credential string) (tools.Principal, error) {
	credential = strings.TrimSpace(credential)
	if !identity.IsGrantCredential(credential) {
		return tools.Principal{}, core.ErrUnauthorized
	}
	viewer, scopes, err := p.opts.Grants.ResolveScoped(ctx, identity.SurfaceMCP, credential)
	if err != nil || viewer == nil || !viewer.IsOwner {
		return tools.Principal{}, core.ErrUnauthorized
	}
	return tools.Principal{ViewerID: viewer.ID, IsOwner: true, Surface: identity.SurfaceMCP, Scopes: scopes}, nil
}

func (p *Pairing) owner(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := p.opts.Authorize(r.Context())
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "owner sign-in required"})
		return 0, false
	}
	return id, true
}

func (p *Pairing) handleClients(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := p.owner(w, r)
	if !ok {
		return
	}
	records, err := p.opts.Grants.ListSurface(r.Context(), ownerID, identity.SurfaceMCP)
	if err != nil {
		p.log.Warn("mcp clients list failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list paired apps"})
		return
	}
	clients := make([]any, 0, len(records))
	for _, rec := range records {
		name := rec.Client
		if name == "" {
			name = "AI app"
		}
		clients = append(clients, map[string]any{
			"id":         rec.Hash,
			"name":       name,
			"scopes":     rec.Scopes,
			"createdAt":  time.UnixMilli(rec.Created).UTC().Format(time.RFC3339),
			"lastSeenAt": time.UnixMilli(rec.LastUsed).UTC().Format(time.RFC3339),
			"expiresAt":  time.UnixMilli(rec.Expires).UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

func (p *Pairing) handleRevoke(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := p.owner(w, r)
	if !ok {
		return
	}
	revoked, err := p.opts.Grants.RevokeSurface(r.Context(), ownerID, identity.SurfaceMCP, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid client id"})
		return
	}
	if !revoked {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Paired app not found."})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// allowAttempt bounds OAuth requests to 10 per address per minute.
func (p *Pairing) allowAttempt(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.attempts[host]; !exists && len(p.attempts) >= 4096 {
		// Evict expired buckets, but never let arbitrary addresses allocate
		// unbounded state or bypass a live bucket's limit.
		for key, attempts := range p.attempts {
			if len(attempts) == 0 || now.Sub(attempts[len(attempts)-1]) >= time.Minute {
				delete(p.attempts, key)
			}
		}
		if len(p.attempts) >= 4096 {
			return false
		}
	}
	recent := p.attempts[host][:0]
	for _, t := range p.attempts[host] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 10 {
		p.attempts[host] = recent
		return false
	}
	p.attempts[host] = append(recent, now)
	return true
}

func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpjson.Write(w, status, v)
}
