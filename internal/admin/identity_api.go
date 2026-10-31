package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
)

type localLogin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) localLogin(w http.ResponseWriter, r *http.Request, surface string) {
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	if h.deps.LocalIdentity == nil || h.deps.IdentityGrants == nil || h.deps.IdentityLimiter == nil {
		writeError(w, 503, "unavailable", "sign in unavailable")
		return
	}
	var body localLogin
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
	allowed, err := h.deps.IdentityLimiter.Allow(r.Context(), body.Username, ip)
	if err != nil || !allowed {
		writeError(w, 429, "rate_limit", "try again later")
		return
	}
	viewerID, identityID, err := h.deps.LocalIdentity.Authenticate(r.Context(), body.Username, body.Password)
	if err != nil {
		writeError(w, 401, "unauthorized", "invalid credentials")
		return
	}
	issue := h.deps.IdentityGrants.IssueLogin
	if surface == "owner" {
		issue = h.deps.IdentityGrants.IssueOwnerLogin
	}
	token, err := issue(r.Context(), viewerID, &identityID, surface, core.Device{Client: "Ether browser"})
	if err != nil {
		writeError(w, 401, "unauthorized", "invalid credentials")
		return
	}
	if surface == "owner" {
		u, e := h.deps.IdentityGrants.Resolve(r.Context(), surface, token)
		if e != nil || !u.IsOwner {
			sum := sha256.Sum256([]byte(token))
			_ = h.deps.IdentityGrants.RevokeGrant(r.Context(), viewerID, hex.EncodeToString(sum[:]))
			writeError(w, 401, "unauthorized", "invalid credentials")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"token": token})
}
func (h *Handler) handleLocalOwnerLogin(w http.ResponseWriter, r *http.Request) {
	h.localLogin(w, r, "owner")
}
func (h *Handler) handleLocalViewerLogin(w http.ResponseWriter, r *http.Request) {
	h.localLogin(w, r, "viewer-portal")
}
func (h *Handler) handleLocalSetup(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) || !setupOpen(h.setupClosed(r), r, h.proxyPrefixes()...) {
		writeError(w, 403, "forbidden", "setup is local only")
		return
	}
	if h.deps.LocalIdentity == nil || h.deps.RefreshViewers == nil {
		writeError(w, 503, "unavailable", "setup unavailable")
		return
	}
	var body localLogin
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	_, err := h.deps.LocalIdentity.CreateOwner(r.Context(), body.Username, body.Password)
	if err != nil {
		writeError(w, 400, "invalid", "could not create owner")
		return
	}
	if err = h.deps.RefreshViewers(r.Context()); err != nil {
		writeError(w, 500, "unavailable", "could not refresh viewers")
		return
	}
	h.localLoginBody(w, r, body)
}
func (h *Handler) localLoginBody(w http.ResponseWriter, r *http.Request, body localLogin) {
	viewerID, identityID, err := h.deps.LocalIdentity.Authenticate(r.Context(), body.Username, body.Password)
	if err != nil || h.deps.IdentityGrants == nil {
		writeError(w, 500, "unavailable", "sign in unavailable")
		return
	}
	token, err := h.deps.IdentityGrants.IssueOwnerLogin(r.Context(), viewerID, &identityID, "owner", core.Device{Client: "Ether browser"})
	if err != nil {
		writeError(w, 500, "unavailable", "sign in unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"token": token})
}
func (h *Handler) portalViewer(r *http.Request) (*core.Viewer, error) {
	if h.deps.IdentityGrants == nil {
		return nil, core.ErrUnauthorized
	}
	return h.deps.IdentityGrants.Resolve(r.Context(), "viewer-portal", tokenFromRequest(r))
}
func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	u, err := h.portalViewer(r)
	if err != nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	writeJSON(w, 200, u)
}
func (h *Handler) handleApproveQuickConnect(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	u, err := h.portalViewer(r)
	if err != nil || h.deps.QuickConnect == nil || h.deps.IdentityLimiter == nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
	ok, err := h.deps.IdentityLimiter.Allow(r.Context(), "quickconnect", ip)
	if err != nil || !ok {
		writeError(w, 429, "rate_limit", "try again later")
		return
	}
	if err = h.deps.QuickConnect.Approve(r.Context(), body.Code, u.ID); err != nil {
		if errors.Is(err, identity.ErrQuickConnectExpired) {
			writeError(w, 400, "expired", "code expired")
			return
		}
		writeError(w, 400, "invalid", "invalid code")
		return
	}
	writeJSON(w, 200, quickConnectApproval(r, h, body.Code, u.ID))
}

func quickConnectApproval(r *http.Request, h *Handler, code string, viewerID int64) map[string]any {
	out := map[string]any{"ok": true}
	target, err := h.deps.QuickConnect.ApprovedTarget(r.Context(), code, viewerID)
	if err != nil {
		return out
	}
	device := strings.TrimSpace(target.DeviceName)
	if device == "" {
		device = strings.TrimSpace(target.Product)
	}
	if device == "" {
		device = strings.TrimSpace(target.Client)
	}
	if device != "" {
		out["device"] = device
	}
	if app := strings.TrimSpace(target.Surface); app != "" {
		out["app"] = app
	}
	return out
}

func (h *Handler) handlePortalPIN(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	if h.deps.SignIn == nil {
		writeError(w, 503, "unavailable", "sign in unavailable")
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	id, code, url, err := h.deps.SignIn.StartPIN(r.Context(), scheme+"://"+r.Host+"/ether/me")
	if err != nil {
		writeError(w, 502, "plextv", "sign in unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"handle": h.portalPins.add(id), "code": code, "authUrl": url})
}
func (h *Handler) handlePortalPINPoll(w http.ResponseWriter, r *http.Request) {
	id, ok := h.portalPins.get(r.PathValue("handle"))
	if !ok || h.deps.SignIn == nil || h.deps.AuthenticatePlex == nil || h.deps.IdentityGrants == nil {
		writeError(w, 404, "expired", "sign in expired")
		return
	}
	token, err := h.deps.SignIn.PINToken(r.Context(), id)
	if err != nil {
		writeError(w, 502, "plextv", "sign in unavailable")
		return
	}
	if token == "" {
		writeJSON(w, 200, map[string]any{"done": false})
		return
	}
	h.portalPins.remove(r.PathValue("handle"))
	u, err := h.deps.AuthenticatePlex(r.Context(), token)
	if err != nil {
		writeError(w, 401, "unauthorized", "sign in rejected")
		return
	}
	grant, err := h.deps.IdentityGrants.IssueLogin(r.Context(), u.ID, nil, "viewer-portal", core.Device{Client: "Ether browser"})
	if err != nil {
		writeError(w, 401, "unauthorized", "sign in rejected")
		return
	}
	writeJSON(w, 200, map[string]any{"done": true, "token": grant})
}

func (h *Handler) handleMeIdentities(w http.ResponseWriter, r *http.Request) {
	u, err := h.portalViewer(r)
	if err != nil || h.deps.LocalIdentity == nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	items, err := h.deps.LocalIdentity.Links(r.Context(), u.ID)
	if err != nil {
		writeError(w, 500, "unavailable", "unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"identities": items})
}
func (h *Handler) handleMePassword(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	u, err := h.portalViewer(r)
	if err != nil || h.deps.LocalIdentity == nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	var body localLogin
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	if err = h.deps.LocalIdentity.UpsertPassword(r.Context(), u.ID, body.Username, body.Password); err != nil {
		writeError(w, 400, "invalid", "could not set password")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (h *Handler) handleMeDevices(w http.ResponseWriter, r *http.Request) {
	u, err := h.portalViewer(r)
	if err != nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	items, err := h.deps.IdentityGrants.List(r.Context(), u.ID)
	if err != nil {
		writeError(w, 500, "unavailable", "unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"devices": items})
}
func (h *Handler) handleMeRevoke(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	u, err := h.portalViewer(r)
	if err != nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	var b struct {
		Hash string `json:"hash"`
	}
	if decodeJSON(r, &b) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	if err = h.deps.IdentityGrants.RevokeGrant(r.Context(), u.ID, b.Hash); err != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *Handler) handleOwnerQuickConnect(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.QuickConnect == nil || h.deps.IdentityLimiter == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid", "invalid viewer")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
	ok, err := h.deps.IdentityLimiter.Allow(r.Context(), "quickconnect", ip)
	if err != nil || !ok {
		writeError(w, 429, "rate_limit", "try again later")
		return
	}
	if err = h.deps.QuickConnect.Approve(r.Context(), body.Code, id); err != nil {
		if errors.Is(err, identity.ErrQuickConnectExpired) {
			writeError(w, 400, "expired", "code expired")
			return
		}
		writeError(w, 400, "invalid", "invalid code")
		return
	}
	writeJSON(w, 200, quickConnectApproval(r, h, body.Code, id))
}

func (h *Handler) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	u, err := h.portalViewer(r)
	if err != nil || h.deps.Invites == nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	secret, err := h.deps.Invites.Create(r.Context(), u.ID)
	if err != nil {
		writeError(w, 500, "unavailable", "unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"url": "/ether/me#invite=" + secret})
}
func (h *Handler) handleInviteRedeem(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	if h.deps.Invites == nil || h.deps.IdentityLimiter == nil {
		writeError(w, 503, "unavailable", "unavailable")
		return
	}
	ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
	ok, err := h.deps.IdentityLimiter.Allow(r.Context(), "invite", ip)
	if err != nil || !ok {
		writeError(w, 429, "rate_limit", "try again later")
		return
	}
	var body struct {
		Secret string `json:"secret"`
	}
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	token, err := h.deps.Invites.Redeem(r.Context(), body.Secret)
	if err != nil {
		writeError(w, 401, "unauthorized", "invalid invite")
		return
	}
	writeJSON(w, 200, map[string]string{"token": token})
}

func (h *Handler) ownerIdentityManagement(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.LocalIdentity == nil || h.deps.IdentityGrants == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid", "invalid viewer")
		return
	}
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/identities"):
		items, e := h.deps.LocalIdentity.Links(r.Context(), id)
		if e != nil {
			writeError(w, 500, "unavailable", "unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"identities": items})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/devices"):
		items, e := h.deps.IdentityGrants.List(r.Context(), id)
		if e != nil {
			writeError(w, 500, "unavailable", "unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"devices": items})
	case strings.HasSuffix(r.URL.Path, "/password"):
		var body localLogin
		if decodeJSON(r, &body) != nil {
			writeError(w, 400, "invalid", "invalid request")
			return
		}
		if err = h.deps.LocalIdentity.UpsertPassword(r.Context(), id, body.Username, body.Password); err != nil {
			writeError(w, 400, "invalid", "could not set password")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case strings.HasSuffix(r.URL.Path, "/devices/revoke"):
		var body struct {
			Hash string `json:"hash"`
		}
		if decodeJSON(r, &body) != nil {
			writeError(w, 400, "invalid", "invalid request")
			return
		}
		if err = h.deps.IdentityGrants.RevokeGrant(r.Context(), id, body.Hash); err != nil {
			writeError(w, 400, "invalid", "invalid request")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case strings.HasSuffix(r.URL.Path, "/invite"):
		if h.deps.Invites == nil {
			writeError(w, 503, "unavailable", "unavailable")
			return
		}
		secret, e := h.deps.Invites.Create(r.Context(), id)
		if e != nil {
			writeError(w, 400, "invalid", "could not create invite")
			return
		}
		writeJSON(w, 200, map[string]string{"url": "/ether/me#invite=" + secret})
	default:
		writeError(w, 404, "not_found", "not found")
	}
}

func (h *Handler) limitIdentityAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.deps.IdentityLimiter == nil {
			writeError(w, 503, "unavailable", "sign in unavailable")
			return
		}
		ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
		ok, err := h.deps.IdentityLimiter.Allow(r.Context(), "plex-pin", ip)
		if err != nil || !ok {
			writeError(w, 429, "rate_limit", "try again later")
			return
		}
		next(w, r)
	}
}

func (h *Handler) handlePlexLinkStart(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	u, err := h.portalViewer(r)
	if err != nil || h.deps.SignIn == nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	id, code, url, err := h.deps.SignIn.StartPIN(r.Context(), scheme+"://"+r.Host+"/ether/me")
	if err != nil {
		writeError(w, 502, "plextv", "sign in unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"handle": h.linkPins.addLinked(id, u.ID), "code": code, "authUrl": url})
}
func (h *Handler) handlePlexLinkPoll(w http.ResponseWriter, r *http.Request) {
	u, err := h.portalViewer(r)
	if err != nil || h.deps.SignIn == nil || h.deps.LinkPlex == nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	id, ok := h.linkPins.linked(r.PathValue("handle"), u.ID)
	if !ok {
		writeError(w, 404, "expired", "sign in expired")
		return
	}
	token, err := h.deps.SignIn.PINToken(r.Context(), id)
	if err != nil {
		writeError(w, 502, "plextv", "sign in unavailable")
		return
	}
	if token == "" {
		writeJSON(w, 200, map[string]bool{"done": false})
		return
	}
	h.linkPins.remove(r.PathValue("handle"))
	if err = h.deps.LinkPlex(r.Context(), u.ID, token); err != nil {
		writeError(w, 403, "identity_conflict", "Unable to link Plex identity")
		return
	}
	writeJSON(w, 200, map[string]bool{"done": true})
}

func (h *Handler) handleCreateLocalViewer(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.LocalIdentity == nil || h.deps.RefreshViewers == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	var body localLogin
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	id, err := h.deps.LocalIdentity.CreateViewer(r.Context(), body.Username, body.Password)
	if err != nil {
		writeError(w, 400, "invalid", "could not create viewer")
		return
	}
	if err = h.deps.RefreshViewers(r.Context()); err != nil {
		writeError(w, 500, "unavailable", "could not refresh viewers")
		return
	}
	writeJSON(w, 201, map[string]int64{"id": id})
}
func (h *Handler) handleViewerMembership(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.SetViewerActive == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid", "invalid viewer")
		return
	}
	var body struct {
		Active *bool `json:"active"`
	}
	if decodeJSON(r, &body) != nil || body.Active == nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	if err = h.deps.SetViewerActive(r.Context(), id, *body.Active); err != nil {
		writeError(w, 403, "forbidden", "viewer cannot be changed")
		return
	}
	writeJSON(w, 200, map[string]bool{"ownerEnabled": *body.Active})
}

func (h *Handler) handleCreateInviteViewer(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.Invites == nil || h.deps.RefreshViewers == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	var body struct {
		Display string `json:"display"`
	}
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	id, secret, err := h.deps.Invites.CreateViewer(r.Context(), body.Display)
	if err != nil {
		writeError(w, 400, "invalid", "could not create viewer")
		return
	}
	if err = h.deps.RefreshViewers(r.Context()); err != nil {
		writeError(w, 500, "unavailable", "could not refresh viewers")
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "url": "/ether/me#invite=" + secret})
}

// limitIdentityPoll is distinct from sign-in limits: normal two-second PIN
// polling must not exhaust password-login attempts. Key by opaque handle.
func (h *Handler) limitIdentityPoll(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.deps.PollLimiter == nil {
			writeError(w, 503, "unavailable", "polling unavailable")
			return
		}
		ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
		ok, err := h.deps.PollLimiter.Allow(r.Context(), r.PathValue("handle"), ip)
		if err != nil || !ok {
			writeError(w, 429, "rate_limit", "try again later")
			return
		}
		next(w, r)
	}
}
