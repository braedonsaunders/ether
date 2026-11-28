package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// Preserve the migration identity for databases upgraded from code pairing,
	// then remove the obsolete table and any stored Quick Connect secrets.
	store.Register("mcp_pairings", 1, `CREATE TABLE mcp_pairings (code TEXT PRIMARY KEY, secret TEXT NOT NULL, scopes TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', expires INTEGER NOT NULL, token TEXT NOT NULL DEFAULT ''); CREATE INDEX mcp_pairings_expires ON mcp_pairings(expires);`)
	store.Register("mcp_pairings", 2, `DROP TABLE mcp_pairings;`)
	store.Register("mcp_oauth", 1, `
CREATE TABLE mcp_oauth_clients (id TEXT PRIMARY KEY, name TEXT NOT NULL, redirects TEXT NOT NULL, created INTEGER NOT NULL);
CREATE TABLE mcp_oauth_requests (id TEXT PRIMARY KEY, client_id TEXT NOT NULL, redirect_uri TEXT NOT NULL, state TEXT NOT NULL, challenge TEXT NOT NULL, scope TEXT NOT NULL, resource TEXT NOT NULL, expires INTEGER NOT NULL);
CREATE TABLE mcp_oauth_codes (hash TEXT PRIMARY KEY, client_id TEXT NOT NULL, redirect_uri TEXT NOT NULL, challenge TEXT NOT NULL, scope TEXT NOT NULL, resource TEXT NOT NULL, owner_id INTEGER NOT NULL, expires INTEGER NOT NULL);
`)
	// Indexes for PruneOAuth and the registration-cap eviction.
	store.Register("mcp_oauth", 2, `CREATE INDEX mcp_oauth_clients_created ON mcp_oauth_clients(created); CREATE INDEX mcp_oauth_requests_expires ON mcp_oauth_requests(expires); CREATE INDEX mcp_oauth_codes_expires ON mcp_oauth_codes(expires);`)
}

// clientUnusedAge is how long a registered client is kept without a pending
// request, an unexpired code or any grant. Clients register immediately
// before they authorize, so an hour only drops abandoned registrations.
const clientUnusedAge = time.Hour

// evictClients deletes the oldest clients that registered more than
// clientUnusedAge ago and hold nothing: no unexpired authorization request
// or code, and no grant row in any state. Tokens are grants issued to
// device "mcp-<client_id>" (token); a revoked or expired grant still keeps
// its client until identity's Prune removes the grant a week after it died,
// so an app re-authorizing with its remembered client_id keeps working.
// Last placeholder is the batch limit.
const evictClients = `DELETE FROM mcp_oauth_clients WHERE id IN (SELECT id FROM mcp_oauth_clients WHERE created<? AND id NOT IN (SELECT client_id FROM mcp_oauth_requests WHERE expires>?) AND id NOT IN (SELECT client_id FROM mcp_oauth_codes WHERE expires>?) AND NOT EXISTS (SELECT 1 FROM identity_grants g WHERE g.device_id='mcp-'||mcp_oauth_clients.id AND g.surface=?) ORDER BY created LIMIT ?)`

// PruneOAuth deletes expired authorization requests and codes and evicts
// unused clients (see evictClients), reporting the rows removed. Requests
// and codes are otherwise only cleared by the next authorize or approval.
func PruneOAuth(ctx context.Context, db *sql.DB) (int64, error) {
	now := time.Now().UnixMilli()
	var total int64
	for _, s := range []struct {
		del  string
		args []any
	}{
		{`DELETE FROM mcp_oauth_requests WHERE id IN (SELECT id FROM mcp_oauth_requests WHERE expires<=? ORDER BY expires LIMIT ?)`, []any{now}},
		{`DELETE FROM mcp_oauth_codes WHERE hash IN (SELECT hash FROM mcp_oauth_codes WHERE expires<=? ORDER BY expires LIMIT ?)`, []any{now}},
		{evictClients, []any{now - clientUnusedAge.Milliseconds(), now, now, identity.SurfaceMCP}},
	} {
		n, err := store.PruneBatched(ctx, db, s.del, s.args...)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func randomURL() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hashCode(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func validRedirect(s string) bool {
	u, e := url.Parse(s)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "localhost" || h == "127.0.0.1"
}
func (p *Pairing) OAuthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if v := checkBoundary(r, p.opts.AllowedHosts, nil); v != nil {
			http.Error(w, v.message, v.status)
			return
		}
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/ether/mcp":
			if r.Method != http.MethodGet {
				w.WriteHeader(405)
				return
			}
			writeJSON(w, 200, map[string]any{"resource": absoluteURL(r, "/ether/mcp"), "authorization_servers": []string{absoluteURL(r, "")}, "scopes_supported": []string{"read", "full"}, "bearer_methods_supported": []string{"header"}})
		case "/.well-known/oauth-authorization-server":
			if r.Method != http.MethodGet {
				w.WriteHeader(405)
				return
			}
			writeJSON(w, 200, map[string]any{"issuer": absoluteURL(r, ""), "authorization_endpoint": absoluteURL(r, "/ether/oauth/authorize"), "token_endpoint": absoluteURL(r, "/ether/oauth/token"), "registration_endpoint": absoluteURL(r, "/ether/oauth/register"), "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"read", "full"}})
		case "/ether/oauth/register":
			p.register(w, r)
		case "/ether/oauth/authorize":
			p.authorize(w, r)
		case "/ether/oauth/token":
			p.token(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
func (p *Pairing) attemptKey(r *http.Request) string {
	if p.opts.ClientIP != nil {
		if ip, ok := p.opts.ClientIP(r); ok && ip.IsValid() {
			return ip.String()
		}
	}
	return r.RemoteAddr
}

func (p *Pairing) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if !p.allowAttempt(p.attemptKey(r)) {
		http.Error(w, "rate limited", 429)
		return
	}
	var b struct {
		Name      string   `json:"client_name"`
		Redirects []string `json:"redirect_uris"`
		Auth      string   `json:"token_endpoint_auth_method"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&b) != nil || len(b.Redirects) == 0 || len(b.Redirects) > 10 || (b.Auth != "" && b.Auth != "none") {
		oauthError(w, "invalid_client_metadata")
		return
	}
	for _, u := range b.Redirects {
		if len(u) > 2048 || !validRedirect(u) {
			oauthError(w, "invalid_redirect_uri")
			return
		}
	}
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = "AI app"
	}
	if len(name) > 80 {
		oauthError(w, "invalid_client_metadata")
		return
	}
	id, err := randomURL()
	if err != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	data, _ := json.Marshal(b.Redirects)
	// Bound durable registrations as well as request buckets. At the cap,
	// evict a small batch of unused clients and try once more.
	now := p.now().UnixMilli()
	insert := func() (int64, error) {
		result, err := p.opts.DB.ExecContext(r.Context(), `INSERT INTO mcp_oauth_clients(id,name,redirects,created) SELECT ?,?,?,? WHERE (SELECT COUNT(*) FROM mcp_oauth_clients)<4096`, id, name, string(data), now)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
	n, err := insert()
	if err == nil && n != 1 {
		if _, err = p.opts.DB.ExecContext(r.Context(), evictClients, now-clientUnusedAge.Milliseconds(), now, now, identity.SurfaceMCP, 64); err == nil {
			n, err = insert()
		}
	}
	if err != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	if n != 1 {
		http.Error(w, "registration limit reached", 429)
		return
	}
	writeJSON(w, 201, map[string]any{"client_id": id, "client_name": name, "redirect_uris": b.Redirects, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code"}, "response_types": []string{"code"}})
}
func oauthError(w http.ResponseWriter, code string) { writeJSON(w, 400, map[string]any{"error": code}) }

// canonicalScope parses an RFC 6749 §3.3 space-delimited scope list
// (clients built from scopes_supported ask for "read full", #651).
// "full" includes "read", so a valid list collapses to that one grant;
// any unknown scope rejects the request rather than being dropped.
// An absent scope keeps the historical default of "full".
func canonicalScope(raw string) (string, bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "full", true
	}
	granted := "read"
	for _, f := range fields {
		switch f {
		case "read":
		case "full":
			granted = "full"
		default:
			return "", false
		}
	}
	return granted, true
}
func (p *Pairing) authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if !p.allowAttempt(p.attemptKey(r)) {
		http.Error(w, "rate limited", 429)
		return
	}
	q := r.URL.Query()
	client, redirect, challenge := q.Get("client_id"), q.Get("redirect_uri"), q.Get("code_challenge")
	scope, scopeOK := canonicalScope(q.Get("scope"))
	resource := q.Get("resource")
	if resource == "" {
		resource = absoluteURL(r, "/ether/mcp")
	}
	var redirects string
	err := p.opts.DB.QueryRowContext(r.Context(), `SELECT redirects FROM mcp_oauth_clients WHERE id=?`, client).Scan(&redirects)
	var uris []string
	_ = json.Unmarshal([]byte(redirects), &uris)
	found := false
	for _, u := range uris {
		if u == redirect {
			found = true
		}
	}
	decoded, e := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || !found || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || e != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != challenge || !scopeOK || resource != absoluteURL(r, "/ether/mcp") || len(q.Get("state")) > 1024 {
		oauthError(w, "invalid_request")
		return
	}
	id, e := randomURL()
	if e != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	now := p.now().UnixMilli()
	_, _ = p.opts.DB.ExecContext(r.Context(), `DELETE FROM mcp_oauth_requests WHERE expires<=?`, now)
	result, e := p.opts.DB.ExecContext(r.Context(), `INSERT INTO mcp_oauth_requests(id,client_id,redirect_uri,state,challenge,scope,resource,expires) SELECT ?,?,?,?,?,?,?,? WHERE (SELECT COUNT(*) FROM mcp_oauth_requests)<4096`, id, client, redirect, q.Get("state"), challenge, scope, resource, p.now().Add(10*time.Minute).UnixMilli())
	if e != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		http.Error(w, "request limit reached", http.StatusTooManyRequests)
		return
	}
	http.Redirect(w, r, "/ether/mcp/authorize?request="+url.QueryEscape(id), http.StatusSeeOther)
}
func (p *Pairing) authorizationDetails(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.owner(w, r); !ok {
		return
	}
	var name, scope, redirectURI string
	err := p.opts.DB.QueryRowContext(r.Context(), `SELECT c.name,q.scope,q.redirect_uri FROM mcp_oauth_requests q JOIN mcp_oauth_clients c ON c.id=q.client_id WHERE q.id=? AND q.expires>?`, r.URL.Query().Get("request"), p.now().UnixMilli()).Scan(&name, &scope, &redirectURI)
	if err != nil {
		http.Error(w, "request expired", 404)
		return
	}
	var redirectHost string
	var redirectLocal bool
	if u, err := url.Parse(redirectURI); err == nil && u.Host != "" {
		redirectHost = u.Host
		h := strings.ToLower(u.Hostname())
		redirectLocal = h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	writeJSON(w, 200, map[string]any{
		"name":          name,
		"scope":         scope,
		"redirectHost":  redirectHost,
		"redirectLocal": redirectLocal,
	})
}
func (p *Pairing) authorizationDecision(w http.ResponseWriter, r *http.Request) {
	owner, ok := p.owner(w, r)
	if !ok {
		return
	}
	var b struct {
		Request string `json:"request"`
		Approve bool   `json:"approve"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&b) != nil {
		oauthError(w, "invalid_request")
		return
	}
	var client, redirect, state, challenge, scope, resource string
	err := p.opts.DB.QueryRowContext(r.Context(), `DELETE FROM mcp_oauth_requests WHERE id=? AND expires>? RETURNING client_id,redirect_uri,state,challenge,scope,resource`, b.Request, p.now().UnixMilli()).Scan(&client, &redirect, &state, &challenge, &scope, &resource)
	if err != nil {
		oauthError(w, "invalid_request")
		return
	}
	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Del("code")
	q.Del("error")
	q.Del("state")
	if state != "" {
		q.Set("state", state)
	}
	if !b.Approve {
		q.Set("error", "access_denied")
	} else {
		code, e := randomURL()
		if e != nil {
			http.Error(w, "unavailable", 500)
			return
		}
		_, _ = p.opts.DB.ExecContext(r.Context(), `DELETE FROM mcp_oauth_codes WHERE expires<=?`, p.now().UnixMilli())
		_, e = p.opts.DB.ExecContext(r.Context(), `INSERT INTO mcp_oauth_codes(hash,client_id,redirect_uri,challenge,scope,resource,owner_id,expires) VALUES(?,?,?,?,?,?,?,?)`, hashCode(code), client, redirect, challenge, scope, resource, owner, p.now().Add(2*time.Minute).UnixMilli())
		if e != nil {
			http.Error(w, "unavailable", 500)
			return
		}
		q.Set("code", code)
	}
	u.RawQuery = q.Encode()
	writeJSON(w, 200, map[string]any{"redirect": u.String()})
}
func (p *Pairing) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if !p.allowAttempt(p.attemptKey(r)) {
		http.Error(w, "rate limited", 429)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if r.ParseForm() != nil {
		oauthError(w, "invalid_request")
		return
	}
	verifier := r.PostForm.Get("code_verifier")
	valid := len(verifier) >= 43 && len(verifier) <= 128
	for _, c := range verifier {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
			valid = false
		}
	}
	if !valid || r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, "invalid_grant")
		return
	}
	client, redirect, resource := r.PostForm.Get("client_id"), r.PostForm.Get("redirect_uri"), r.PostForm.Get("resource")
	if resource == "" {
		resource = absoluteURL(r, "/ether/mcp")
	}
	challenge := hashCode(verifier)
	var scope, name string
	var owner int64
	err := p.opts.DB.QueryRowContext(r.Context(), `DELETE FROM mcp_oauth_codes WHERE hash=? AND client_id=? AND redirect_uri=? AND resource=? AND challenge=? AND expires>? RETURNING owner_id,scope`, hashCode(r.PostForm.Get("code")), client, redirect, resource, challenge, p.now().UnixMilli()).Scan(&owner, &scope)
	if err != nil {
		oauthError(w, "invalid_grant")
		return
	}
	if err = p.opts.DB.QueryRowContext(r.Context(), `SELECT name FROM mcp_oauth_clients WHERE id=?`, client).Scan(&name); err != nil {
		oauthError(w, "invalid_grant")
		return
	}
	token, err := p.opts.Grants.IssueScoped(r.Context(), owner, identity.SurfaceMCP, core.Device{ID: "mcp-" + client, Client: name, Product: "MCP"}, []string{scope})
	if err != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"access_token": token, "token_type": "Bearer", "scope": scope})
}
