package admin

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/oidcsignin"
)

// OIDCSignIn runs single sign-on flows (satisfied by *oidcsignin.Client).
type OIDCSignIn interface {
	Check(ctx context.Context, s oidcsignin.Settings) error
	Start(ctx context.Context, s oidcsignin.Settings, redirectURL string, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Started, error)
	Callback(ctx context.Context, state, binding, code, providerError string) error
	Take(handle string, purpose oidcsignin.Purpose, viewerID int64) (oidcsignin.Identity, bool, error)
}

// oidcCallbackPath is the redirect URI path to register with the provider,
// below each address people open Ether at.
const oidcCallbackPath = "/ether/api/oidc/callback"

func oidcSettings(c config.OIDC) oidcsignin.Settings {
	return oidcsignin.Settings{Issuer: c.Issuer, ClientID: c.ClientID, ClientSecret: c.ClientSecret,
		Groups: strings.TrimSpace(c.RequiredGroup) != ""}
}

// oidcEnabled reports whether single sign-on is switched on, configured and
// backed by the identity services it needs.
func (h *Handler) oidcEnabled() bool {
	c := h.deps.Config.Get().OIDC
	return c.Enabled && oidcSettings(c).Usable() && h.deps.OIDC != nil &&
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
	if !h.oidcEnabled() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "single sign-on is not set up")
		return
	}
	redirect := h.oidcRedirectURL(r)
	started, err := h.deps.OIDC.Start(r.Context(), oidcSettings(h.deps.Config.Get().OIDC), redirect, purpose, viewerID)
	switch {
	case errors.Is(err, oidcsignin.ErrBusy):
		writeError(w, http.StatusTooManyRequests, "rate_limit", "try again later")
		return
	case err != nil:
		h.log.Warn("oidc: start sign-in", "err", err)
		writeError(w, http.StatusBadGateway, "oidc_unavailable", "the sign-in provider could not be reached")
		return
	}
	setOIDCBinding(w, redirect, started.State, started.Binding, int(oidcsignin.FlowTTL.Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"handle": started.Handle, "authUrl": started.AuthURL})
}

// oidcBindingCookie names the cookie that ties one flow to the browser that
// started it. It only proves "same browser" at the callback; it is never a
// credential and no other route reads it.
func oidcBindingCookie(state string) string {
	if len(state) > 16 {
		state = state[:16]
	}
	return "ether_oidc_" + state
}

// setOIDCBinding sets (maxAge > 0) or clears the binding cookie, scoped to the
// callback path the provider returns the browser to. SameSite=Lax keeps it
// on that top-level redirect and off cross-site subrequests.
func setOIDCBinding(w http.ResponseWriter, redirect, state, binding string, maxAge int) {
	path := oidcCallbackPath
	secure := false
	if u, err := url.Parse(redirect); err == nil {
		if u.Path != "" {
			path = u.Path
		}
		secure = u.Scheme == "https"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcBindingCookie(state),
		Value:    binding,
		Path:     path,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
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
	h.oidcStart(w, r, oidcsignin.PurposeLink, u.ID)
}

// handleOIDCCallback receives the provider's redirect. It finishes the
// verification but hands nothing to this browser tab: the tab that started
// the sign-in collects the result with its handle.
func (h *Handler) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	binding := ""
	if c, e := r.Cookie(oidcBindingCookie(state)); e == nil {
		binding = c.Value
	}
	err := errors.New("unavailable")
	if h.deps.OIDC != nil && state != "" {
		err = h.deps.OIDC.Callback(r.Context(), state, binding, q.Get("code"), q.Get("error"))
	}
	if binding != "" {
		setOIDCBinding(w, h.oidcRedirectURL(r), state, "", -1)
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
	if !h.oidcEnabled() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "single sign-on is not set up")
		return
	}
	who, done, err := h.deps.OIDC.Take(r.PathValue("handle"), purpose, 0)
	if !h.oidcPending(w, done, err) {
		return
	}
	ip, _ := identity.ClientNet(r, h.proxyPrefixes()...)
	viewerID, identityID, err := h.deps.LocalIdentity.OIDCViewer(r.Context(), who.Subject)
	if err != nil && purpose == oidcsignin.PurposePortal {
		viewerID, identityID, err = h.oidcAutoViewer(r.Context(), who, ip)
	}
	if err != nil {
		h.emitLoginFailed(who.Display, "oidc", "not_linked", "Ether browser", "Web", ip.String())
		writeError(w, http.StatusForbidden, "not_linked", "That account isn't linked to anyone here. Sign in another way and link it from your account page first.")
		return
	}
	token, refused := h.issueBrowserLogin(r, viewerID, &identityID, surface)
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
func (h *Handler) oidcAutoViewer(ctx context.Context, who oidcsignin.Identity, ip netip.Addr) (int64, int64, error) {
	c := h.deps.Config.Get().OIDC
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
		if id, err := h.deps.LocalIdentity.MatchOIDCViewer(ctx, match); err == nil {
			if err = h.deps.LocalIdentity.LinkOIDC(ctx, id, who.Subject, who.Display); err != nil {
				return 0, 0, err
			}
			h.log.Info("oidc: linked a first sign-in to an existing viewer", "viewer", id, "by_email", match.Email != "", "by_username", match.Username != "", "remote", ip.String())
			return h.deps.LocalIdentity.OIDCViewer(ctx, who.Subject)
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
	if !h.oidcEnabled() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "single sign-on is not set up")
		return
	}
	who, done, err := h.deps.OIDC.Take(r.PathValue("handle"), oidcsignin.PurposeLink, u.ID)
	if !h.oidcPending(w, done, err) {
		return
	}
	if err = h.deps.LocalIdentity.LinkOIDC(r.Context(), u.ID, who.Subject, who.Display); err != nil {
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
	if err != nil || h.deps.LocalIdentity == nil {
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
	if err = h.deps.LocalIdentity.UnlinkOIDC(r.Context(), u.ID, body.ID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such sign-in method")
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
	h.runTest(w, r, func(ctx context.Context) error { return h.deps.OIDC.Check(ctx, oidcSettings(cfg)) })
}
