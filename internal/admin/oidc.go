package admin

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/oidcsignin"
)

// OIDCSignIn runs single sign-on flows (satisfied by *oidcsignin.Client).
type OIDCSignIn interface {
	Check(ctx context.Context, s oidcsignin.Settings) error
	Start(ctx context.Context, s oidcsignin.Settings, redirectURL string, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Started, error)
	Callback(ctx context.Context, state, binding, code, providerError string, revision uint64) error
	Take(handle, binding string, revision uint64, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Identity, bool, error)
}

// oidcCallbackPath is the redirect URI path to register with the provider,
// below each address people open Ether at.
const oidcCallbackPath = "/ether/api/oidc/callback"

func oidcSettings(c config.OIDC, revision uint64) oidcsignin.Settings {
	return oidcsignin.Settings{Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: c.ClientSecret,
		Revision: revision, Scopes: strings.Fields(c.Scopes), GroupsClaim: c.GroupsClaim}
}

// oidcEnabled reports whether single sign-on is switched on, configured and
// backed by the identity services it needs.
func (h *Handler) oidcEnabled() bool {
	c, _ := h.deps.Config.OIDCSettings()
	return h.oidcConfigured(c)
}

func (h *Handler) oidcConfigured(c config.OIDC) bool {
	return c.Enabled && oidcSettings(c, 0).Usable() && h.deps.OIDC != nil &&
		h.deps.LocalIdentity != nil && h.deps.IdentityGrants != nil
}

func (h *Handler) oidcDisplayName() string {
	return strings.TrimSpace(h.deps.Config.Get().OIDC.DisplayName)
}

// oidcRedirectURL is the callback as the browser addresses this server, so
// the provider returns the person to the host they started on.
func (h *Handler) oidcRedirectURL(r *http.Request) string {
	if origin, ok := clientnet.Origin(r, h.proxyPrefixes()...); ok {
		return origin.BaseURL(oidcCallbackPath)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + oidcCallbackPath
}

func (h *Handler) oidcStart(w http.ResponseWriter, r *http.Request, purpose oidcsignin.Purpose, viewerID int64) {
	cfg, revision := h.deps.Config.OIDCSettings()
	if !h.oidcConfigured(cfg) {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "single sign-on is not set up")
		return
	}
	redirect := h.oidcRedirectURL(r)
	if !strings.HasPrefix(redirect, "https://") {
		writeError(w, http.StatusForbidden, "oidc_requires_https", "open Ether over HTTPS to use single sign-on")
		return
	}
	started, err := h.deps.OIDC.Start(r.Context(), oidcSettings(cfg, revision), redirect, purpose, viewerID)
	switch {
	case errors.Is(err, oidcsignin.ErrBusy):
		writeError(w, http.StatusTooManyRequests, "rate_limit", "try again later")
		return
	case err != nil:
		h.log.Warn("oidc: start sign-in", "err", err)
		writeError(w, http.StatusBadGateway, "oidc_unavailable", "the sign-in provider could not be reached")
		return
	}
	setOIDCBinding(w, started.State, started.Binding, int(oidcsignin.FlowTTL.Seconds()))
	writeJSON(w, http.StatusOK, map[string]string{"handle": started.Handle, "state": started.State, "authUrl": started.AuthURL})
}

// oidcBindingCookie names the cookie that ties one flow to the browser that
// started it. The __Host- prefix prevents sibling domains from setting it.
func oidcBindingCookie(state string) string {
	if len(state) > 16 {
		state = state[:16]
	}
	return "__Host-ether_oidc_" + state
}

// SameSite=Lax sends the cookie on the provider's top-level callback.
func setOIDCBinding(w http.ResponseWriter, state, binding string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcBindingCookie(state),
		Value:    binding,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func oidcBinding(r *http.Request, state string) string {
	if cookie, err := r.Cookie(oidcBindingCookie(state)); err == nil {
		return cookie.Value
	}
	return ""
}

// handleOIDCOwnerStart begins single sign-on for the owner console.
func (h *Handler) handleOIDCOwnerStart(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "csrf", "request rejected")
		return
	}
	h.oidcStart(w, r, oidcsignin.PurposeOwner, 0)
}

// handleOIDCPortalStart begins single sign-on for the viewer portal.
func (h *Handler) handleOIDCPortalStart(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "csrf", "request rejected")
		return
	}
	h.oidcStart(w, r, oidcsignin.PurposePortal, 0)
}

// handleOIDCLinkStart lets a signed-in viewer add their provider account as
// a sign-in method.
func (h *Handler) handleOIDCLinkStart(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "csrf", "request rejected")
		return
	}
	u, err := h.portalViewer(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	h.oidcStart(w, r, oidcLinkPurpose(u.IsOwner), u.ID)
}

func oidcLinkPurpose(owner bool) oidcsignin.Purpose {
	if owner {
		return oidcsignin.PurposeOwnerLink
	}
	return oidcsignin.PurposeLink
}

// handleOIDCCallback receives the provider's redirect. It finishes the
// verification but hands nothing to this browser tab: the tab that started
// the sign-in collects the result with its handle.
func (h *Handler) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	cfg, revision := h.deps.Config.OIDCSettings()
	err := oidcsignin.ErrExpired
	if h.oidcConfigured(cfg) && state != "" {
		err = h.deps.OIDC.Callback(r.Context(), state, oidcBinding(r, state), q.Get("code"), q.Get("error"), revision)
	}
	if err != nil && !errors.Is(err, oidcsignin.ErrExpired) {
		h.log.Warn("oidc: sign-in callback", "err", err)
	}
	title, body := "Signed in", "You can close this window and return to Ether."
	status := http.StatusOK
	switch {
	case errors.Is(err, oidcsignin.ErrExpired):
		title, body, status = "Sign-in expired", "Close this window and start again in Ether.", http.StatusBadRequest
	case err != nil:
		title, body, status = "Sign-in failed", "Close this window and try again in Ether.", http.StatusBadRequest
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` +
		html.EscapeString(title) + `</title></head><body><h1>` + html.EscapeString(title) + `</h1><p>` + html.EscapeString(body) + `</p></body></html>`))
}

// handleOIDCOwnerPoll issues a console session once the provider account
// that signed in is linked to the owner.
func (h *Handler) handleOIDCOwnerPoll(w http.ResponseWriter, r *http.Request) {
	h.oidcSignInPoll(w, r, oidcsignin.PurposeOwner, "owner")
}

// handleOIDCPortalPoll issues a viewer portal session once the provider
// account that signed in is linked to an active viewer.
func (h *Handler) handleOIDCPortalPoll(w http.ResponseWriter, r *http.Request) {
	h.oidcSignInPoll(w, r, oidcsignin.PurposePortal, "viewer-portal")
}

func (h *Handler) oidcSignInPoll(w http.ResponseWriter, r *http.Request, purpose oidcsignin.Purpose, surface string) {
	who, cfg, ready := h.oidcTake(w, r, purpose, 0)
	if !ready {
		return
	}
	ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
	viewerID, identityID, err := h.deps.LocalIdentity.OIDCViewer(r.Context(), who.Subject)
	if errors.Is(err, core.ErrUnauthorized) && purpose == oidcsignin.PurposePortal {
		viewerID, identityID, err = h.oidcAutoViewer(r.Context(), cfg, who, ip)
	}
	if err != nil {
		h.emitLoginFailed(who.Display, "oidc", "not_linked", "Ether browser", "Web", ip.String())
		writeError(w, http.StatusForbidden, "not_linked", "That account isn't linked to anyone here. Sign in another way and link it from your account page first.")
		return
	}
	token, refused := h.issueBrowserLogin(r, viewerID, &identityID, surface, true)
	if refused == "owner_link_required" {
		h.emitLoginFailed(who.Display, "oidc", refused, "Ether browser", "Web", ip.String())
		writeError(w, http.StatusForbidden, "not_linked", "Sign in another way and confirm this sign-in method after becoming the owner.")
		return
	}
	if refused == "not_owner" {
		h.emitLoginFailed(who.Display, "oidc", refused, "Ether browser", "Web", ip.String())
		writeError(w, http.StatusForbidden, "not_owner", "That account doesn't own this server. Sign in as the owner.")
		return
	}
	if refused != "" {
		h.emitLoginFailed(who.Display, "oidc", refused, "Ether browser", "Web", ip.String())
		writeError(w, http.StatusUnauthorized, "unauthorized", "sign in rejected")
		return
	}
	h.resetOIDCLimit(r, ip)
	h.emitLoginSucceeded(viewerID, who.Display, "oidc", "Ether browser", "Web", ip.String())
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "token": token})
}

// oidcAutoViewer applies the owner's automatic options to a provider account
// that is not linked yet: link it to a matching viewer, or create one. Neither
// can reach the owner, and both need the required group when one is set.
func (h *Handler) oidcAutoViewer(ctx context.Context, c config.OIDC, who oidcsignin.Identity, ip netip.Addr) (int64, int64, error) {
	if group := strings.TrimSpace(c.RequiredGroup); group != "" && !slices.Contains(who.Groups, group) {
		return 0, 0, identity.ErrNoMatch
	}
	issuer, _, _ := strings.Cut(who.Subject, "|")
	match := identity.OIDCMatch{Issuer: issuer}
	if c.AutoLinkEmail == config.OIDCAutoLinkAny || (c.AutoLinkEmail == config.OIDCAutoLinkVerified && who.EmailVerified) {
		match.Email = who.Email
	}
	if c.AutoLinkUsername {
		match.Username = who.Username
	}
	if match.Email != "" || match.Username != "" {
		id, err := h.deps.LocalIdentity.AutoLinkOIDC(ctx, match, who.Subject, who.Display)
		if err == nil {
			h.log.Info("oidc: linked a first sign-in to an existing viewer", "viewer", id, "by_email", match.Email != "", "by_username", match.Username != "", "remote", ip.String())
			return h.deps.LocalIdentity.OIDCViewer(ctx, who.Subject)
		}
		if !errors.Is(err, identity.ErrNoMatch) {
			return 0, 0, err
		}
	}
	if !c.AutoProvision {
		return 0, 0, identity.ErrNoMatch
	}
	email := ""
	if who.EmailVerified {
		// An unverified address is not stored: a later e-mail match must not
		// trust it.
		email = who.Email
	}
	id, err := h.deps.LocalIdentity.ProvisionOIDCViewer(ctx, who.Subject, who.Display, who.Username, who.Name, email)
	if err != nil {
		return 0, 0, err
	}
	if h.deps.RefreshViewers != nil {
		if err = h.deps.RefreshViewers(ctx); err != nil {
			return 0, 0, err
		}
	}
	h.log.Info("oidc: created a viewer for a first sign-in", "viewer", id, "remote", ip.String())
	return h.deps.LocalIdentity.OIDCViewer(ctx, who.Subject)
}

// handleOIDCLinkPoll links the provider account once the signed-in viewer
// who started the link has finished at the provider.
func (h *Handler) handleOIDCLinkPoll(w http.ResponseWriter, r *http.Request) {
	u, err := h.portalViewer(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	who, _, ready := h.oidcTake(w, r, oidcLinkPurpose(u.IsOwner), u.ID)
	if !ready {
		return
	}
	if err := h.deps.LocalIdentity.LinkOIDC(r.Context(), u.ID, who.Subject, who.Display, u.IsOwner); err != nil {
		if errors.Is(err, identity.ErrIdentityLinked) {
			writeError(w, http.StatusConflict, "identity_conflict", "That account already signs in as someone else.")
			return
		}
		writeError(w, http.StatusForbidden, "identity_conflict", "Unable to link that account")
		return
	}
	ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
	h.resetOIDCLimit(r, ip)
	writeJSON(w, http.StatusOK, map[string]any{"done": true})
}

func (h *Handler) oidcTake(w http.ResponseWriter, r *http.Request, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Identity, config.OIDC, bool) {
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "csrf", "request rejected")
		return oidcsignin.Identity{}, config.OIDC{}, false
	}
	var body struct {
		Handle string `json:"handle"`
		State  string `json:"state"`
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
	err := decodeLoginJSON(w, r, &body)
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil || body.Handle == "" || body.State == "" {
		writeError(w, http.StatusBadRequest, "invalid", "invalid request")
		return oidcsignin.Identity{}, config.OIDC{}, false
	}
	if !h.allowIdentityPoll(w, r, body.Handle) {
		return oidcsignin.Identity{}, config.OIDC{}, false
	}
	cfg, revision := h.deps.Config.OIDCSettings()
	if !h.oidcConfigured(cfg) {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "single sign-on is not set up")
		return oidcsignin.Identity{}, config.OIDC{}, false
	}
	binding := oidcBinding(r, body.State)
	who, done, err := h.deps.OIDC.Take(body.Handle, binding, revision, purpose, viewerID)
	if binding != "" && done {
		setOIDCBinding(w, body.State, "", -1)
	}
	return who, cfg, h.oidcPending(w, done, err)
}

// resetOIDCLimit clears this client's single sign-on start budget after a
// completed sign-in or link, as a correct password does for password login.
func (h *Handler) resetOIDCLimit(r *http.Request, ip netip.Addr) {
	if h.deps.IdentityLimiter != nil {
		_ = h.deps.IdentityLimiter.ResetIdentity(r.Context(), "oidc", ip)
	}
}

// oidcPending answers a poll that has no verified identity yet. It returns
// true only when the flow finished and its identity may be used.
func (h *Handler) oidcPending(w http.ResponseWriter, done bool, err error) bool {
	switch {
	case errors.Is(err, oidcsignin.ErrExpired):
		writeError(w, http.StatusNotFound, "expired", "This sign-in expired. Start again.")
	case err != nil:
		writeError(w, http.StatusUnauthorized, "rejected", "The sign-in was not completed.")
	case !done:
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
	default:
		return true
	}
	return false
}

// handleMeUnlinkIdentity removes one of the signed-in viewer's single
// sign-on links.
func (h *Handler) handleMeUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, http.StatusForbidden, "csrf", "request rejected")
		return
	}
	u, err := h.portalViewer(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if decodeJSON(r, &body) != nil || body.ID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "invalid request")
		return
	}
	err = h.deps.IdentityGrants.UnlinkOIDC(r.Context(), u.ID, body.ID)
	if errors.Is(err, core.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such sign-in method")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "could not disconnect the sign-in method")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTestOIDC checks that the issuer publishes a usable discovery
// document. The client ID and secret are only proven by a real sign-in.
func (h *Handler) handleTestOIDC(w http.ResponseWriter, r *http.Request) {
	prev := h.deps.Config.Get().OIDC
	cfg := redactOIDC(prev)
	if !decodeTestBody(w, r, &cfg) {
		return
	}
	var perrs placeholderErrors
	cfg = perrs.unredactOIDC(cfg, prev)
	if len(perrs) > 0 {
		writeSecretRequired(w, perrs)
		return
	}
	var v validator
	v.oidcIssuer("oidc.issuer", strings.TrimSpace(cfg.Issuer), true)
	if len(v.errs) > 0 {
		writeJSON(w, http.StatusOK, testResult{Error: "The issuer " + v.errs[0].Message + "."})
		return
	}
	if h.deps.OIDC == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "single sign-on is unavailable")
		return
	}
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.OIDC.Check(ctx, oidcSettings(cfg, 0)) })
}
