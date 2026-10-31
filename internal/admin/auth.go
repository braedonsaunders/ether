package admin

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
)

// tokenFromRequest extracts the owner credential from headers. Cookies are
// never consulted (CSRF posture). Query strings are never consulted either,
// so tokens stay out of access logs.
func tokenFromRequest(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get("X-Plex-Token")); t != "" {
		return t
	}
	if t := strings.TrimSpace(r.Header.Get("X-Ether-Admin-Token")); t != "" {
		return t
	}
	if a := strings.TrimSpace(r.Header.Get("Authorization")); a != "" {
		if rest, ok := strings.CutPrefix(a, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func (h *Handler) isOwner(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	if h.deps.IdentityGrants != nil {
		u, err := h.deps.IdentityGrants.Resolve(r.Context(), "owner", token)
		if err == nil {
			return u.IsOwner
		}
	}
	if core.IsGrantCredential(token) {
		return false
	}
	return h.deps.IsOwnerToken != nil && h.deps.IsOwnerToken(r.Context(), token)
}

// isLocalOrLAN classifies the real client behind a trusted proxy.
func isLocalOrLAN(r *http.Request, trusted ...netip.Prefix) bool {
	ip, _ := identity.ClientNet(r, trusted...)
	return ip.IsValid() && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}

// localSetupHosts are DNS suffixes that only resolve on the local network.
var localSetupHosts = []string{".local", ".lan", ".home", ".home.arpa", ".internal", ".localdomain", ".plex.direct"}

// localHost reports whether the Host the browser addressed is an IP
// literal, localhost, a single-label LAN name ("nas") or a name under a
// local-only suffix. Tokenless setup access is limited to such hosts: a web
// page on any public domain can re-point that domain at this server's LAN
// address (DNS rebinding) and then drive the setup API from the owner's
// browser as a same-origin, LAN-peer request.
func localHost(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") {
		return true
	}
	for _, s := range localSetupHosts {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

// SetupOpen is setupOpen for handlers outside the admin API (a failed
// start's recovery page): whether r gets tokenless local-network access
// to a server whose setup is not complete.
func SetupOpen(setupComplete bool, r *http.Request) bool { return setupOpen(setupComplete, r) }

// setupOpen reports whether r gets tokenless access: setup is not complete
// and the browser is on the local network, addressing this server by a
// local name.
func setupOpen(cfgSetupComplete bool, r *http.Request, trusted ...netip.Prefix) bool {
	return !cfgSetupComplete && isLocalOrLAN(r, trusted...) && localHost(r)
}

// requireAuth implements the admin access policy:
//
//   - A valid owner token always passes, from anywhere.
//   - Before setup is closed, requests from localhost/LAN that address a
//     local hostname pass without a token. Setup closes when it is complete
//     or a local owner exists. Remote requests and public DNS names do not.
//   - After setup is closed, a valid owner credential is mandatory.
//   - Mutating requests additionally must pass the CSRF header check.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !csrfOK(r) {
			writeError(w, http.StatusForbidden, "csrf", "mutating requests require the X-Ether-Request: 1 header and a JSON content type")
			return
		}
		token := tokenFromRequest(r)
		setupComplete := h.deps.Config.Get().SetupComplete
		if h.isOwner(r, token) {
			next.ServeHTTP(w, withAccess(r, Access{Owner: true, SetupComplete: setupComplete}))
			return
		}
		if !h.setupClosed(r) {
			if setupOpen(false, r, h.proxyPrefixes()...) {
				next.ServeHTTP(w, withAccess(r, Access{}))
				return
			}
			if isLocalOrLAN(r, h.proxyPrefixes()...) {
				writeError(w, http.StatusForbidden, "setup_local_host", "open setup at this server's IP address or local hostname (for example http://192.168.1.10:32400/ether/) until it is complete")
				return
			}
			writeError(w, http.StatusForbidden, "setup_local_only", "setup is only available from the local network until it is complete")
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="ether-admin"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "sign in with an owner account")
	}
}

// Access is how requireAuth admitted a request: with a validated owner
// token, or (Owner false) through tokenless local-network setup access.
type Access struct {
	Owner         bool
	SetupComplete bool
}

type accessKey struct{}

func withAccess(r *http.Request, a Access) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), accessKey{}, a))
}

// RequestAccess reports how the admin API admitted the request whose
// context is ctx. Only requireAuth sets it, from its own checks, so
// extension handlers can demand more than admission (an owner token, a
// finished setup); ok is false for a request that did not pass through
// requireAuth.
func RequestAccess(ctx context.Context) (a Access, ok bool) {
	a, ok = ctx.Value(accessKey{}).(Access)
	return a, ok
}

// handleBootstrap is intentionally public and minimal: version, whether
// setup is complete, and whether the caller will need a token. It carries
// no secrets and no configuration.
func (h *Handler) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	cfg := h.deps.Config.Get()
	token := tokenFromRequest(r)
	authRequired := h.setupClosed(r)
	authed := h.isOwner(r, token) || setupOpen(authRequired, r, h.proxyPrefixes()...)
	signInMethods := []string{}
	if h.deps.LocalIdentity != nil && h.deps.IdentityGrants != nil && h.deps.IdentityLimiter != nil {
		signInMethods = append(signInMethods, "local")
	}
	if h.deps.SignIn != nil && cfg.Plex.Claimed {
		signInMethods = append(signInMethods, "plex")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"version":         h.version,
		"setupComplete":   cfg.SetupComplete,
		"ownerConfigured": h.deps.OwnerConfigured != nil && h.deps.OwnerConfigured(r.Context()),
		"authenticated":   authed,
		"authRequired":    authRequired,
		"signInMethods":   signInMethods,
		"serverName":      cfg.Server.FriendlyName,
	})
}

func (h *Handler) proxyPrefixes() []netip.Prefix {
	return identity.ProxyPrefixes(h.deps.Config.Get().Server.TrustedProxyCIDRs)
}

// setupClosed closes tokenless access immediately once a local owner exists,
// while leaving Plex's existing claim wizard behavior unchanged.
func (h *Handler) setupClosed(r *http.Request) bool {
	return h.deps.Config.Get().SetupComplete || (h.deps.LocalOwnerConfigured != nil && h.deps.LocalOwnerConfigured(r.Context()))
}
