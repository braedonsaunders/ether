package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// pinHandles maps random handles to plex.tv PIN ids. The browser that
// started a sign-in gets the handle; plex.tv PIN ids are short integers,
// so polling by them would let another client on the network collect the
// owner's token when they approve.
type pinHandles struct {
	mu sync.Mutex
	m  map[string]pinHandle
}

type pinHandle struct {
	viewerID int64
	pinID    string
	expires  time.Time
}

const pinHandleTTL = 15 * time.Minute

func newPINHandles() *pinHandles { return &pinHandles{m: map[string]pinHandle{}} }

func (p *pinHandles) add(pinID string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	handle := hex.EncodeToString(b)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.m {
		if now.After(v.expires) {
			delete(p.m, k)
		}
	}
	p.m[handle] = pinHandle{pinID: pinID, expires: now.Add(pinHandleTTL)}
	return handle
}

func (p *pinHandles) get(handle string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[handle]
	if !ok || time.Now().After(v.expires) {
		delete(p.m, handle)
		return "", false
	}
	return v.pinID, true
}

func (p *pinHandles) remove(handle string) {
	p.mu.Lock()
	delete(p.m, handle)
	p.mu.Unlock()
}

// handleSignInStart begins "Sign in with Plex" for the admin UI.
func (h *Handler) handleSignInStart(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "csrf", "mutating requests require the X-Ether-Request: 1 header and a JSON content type")
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	forward := scheme + "://" + r.Host + "/ether/"
	id, code, authURL, err := h.deps.SignIn.StartPIN(r.Context(), forward)
	if err != nil {
		writeError(w, http.StatusBadGateway, "plextv", "Could not reach plex.tv: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"handle": h.pins.add(id), "code": code, "authUrl": authURL})
}

// handleSignInPoll reports whether the PIN was approved. Only an owner
// sign-in completes, and the browser receives a console session, never the
// plex.tv account token. The handle stays valid until a session is issued,
// so a transient failure can be retried with the same approved PIN.
func (h *Handler) handleSignInPoll(w http.ResponseWriter, r *http.Request) {
	handle := r.PathValue("handle")
	pinID, ok := h.pins.get(handle)
	if !ok {
		writeError(w, http.StatusNotFound, "expired", "This sign-in expired. Start again.")
		return
	}
	token, err := h.deps.SignIn.PINToken(r.Context(), pinID)
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, "plextv", "Could not check the sign-in with plex.tv: "+err.Error())
		return
	case token == "":
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	if !h.isOwner(r, token) {
		h.pins.remove(handle)
		writeError(w, http.StatusForbidden, "not_owner", "That Plex account doesn't own this server. Sign in as the owner.")
		return
	}
	// This is a completed external owner sign-in, not ordinary token use.
	// Record it before exchanging the plex.tv credential for a console session.
	if h.deps.IdentityGrants != nil && h.deps.AuthenticatePlex != nil {
		if viewer, e := h.deps.AuthenticatePlex(r.Context(), token); e == nil && viewer != nil {
			_ = h.deps.IdentityGrants.ViewerTimes().RecordOwnerLogin(r.Context(), viewer.ID)
		}
	}
	if !h.writeSession(w, r, token) {
		return
	}
	h.pins.remove(handle)
}

// handleSessionCreate exchanges an owner credential sent in the request
// headers (a pasted Plex token) for a console session.
func (h *Handler) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "csrf", "mutating requests require the X-Ether-Request: 1 header and a JSON content type")
		return
	}
	credential := tokenFromRequest(r)
	if credential == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid owner X-Plex-Token is required")
		return
	}
	h.writeSession(w, r, credential)
}

// handleSessionDelete signs the console out: the presented session stops
// working on every node. It needs no other credential, and ending an
// unknown or expired session succeeds.
func (h *Handler) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "csrf", "mutating requests require the X-Ether-Request: 1 header and a JSON content type")
		return
	}
	if h.deps.Sessions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if token := tokenFromRequest(r); token != "" {
		if err := h.deps.Sessions.RevokeOwnerSession(r.Context(), token); err != nil {
			writeError(w, http.StatusServiceUnavailable, "session", "Could not sign out: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeSession issues a console session for credential and writes it. It
// reports whether it succeeded.
func (h *Handler) writeSession(w http.ResponseWriter, r *http.Request, credential string) bool {
	if h.deps.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "session", "Console sessions are unavailable.")
		return false
	}
	token, expires, err := h.deps.Sessions.IssueOwnerSession(r.Context(), credential)
	switch {
	case errors.Is(err, core.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", `Bearer realm="ether-admin"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "That token was not accepted as the owner token.")
		return false
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "session", "Could not start a console session: "+err.Error())
		return false
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "token": token, "expiresAt": expires.Unix()})
	return true
}

func (p *pinHandles) addLinked(pinID string, viewerID int64) string {
	handle := p.add(pinID)
	p.mu.Lock()
	v := p.m[handle]
	v.viewerID = viewerID
	p.m[handle] = v
	p.mu.Unlock()
	return handle
}
func (p *pinHandles) linked(handle string, viewerID int64) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[handle]
	if !ok || v.viewerID != viewerID || time.Now().After(v.expires) {
		return "", false
	}
	return v.pinID, true
}
