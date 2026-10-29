package admin

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
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

// handleSignInPoll reports whether the PIN was approved. The token is
// returned only if the account owns this server.
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
	h.pins.remove(handle)
	if !h.isOwner(r, token) {
		writeError(w, http.StatusForbidden, "not_owner", "That Plex account doesn't own this server. Sign in as the owner.")
		return
	}
	// This is a completed external owner sign-in, not ordinary token use.
	// Preserve the existing Plex sign-in response if observation is unavailable.
	if h.deps.IdentityGrants != nil && h.deps.AuthenticatePlex != nil {
		if viewer, e := h.deps.AuthenticatePlex(r.Context(), token); e == nil && viewer != nil {
			_ = h.deps.IdentityGrants.ViewerTimes().RecordOwnerLogin(r.Context(), viewer.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "token": token})
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
