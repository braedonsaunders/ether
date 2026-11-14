package admin

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

func (h *Handler) webPlayer(w http.ResponseWriter, r *http.Request) {
	if h.deps.IdentityGrants == nil || h.deps.Frontends == nil {
		writeError(w, 503, "unavailable", "Web player unavailable")
		return
	}
	var viewer *core.Viewer
	for _, surface := range []string{"owner", "viewer-portal"} {
		v, err := h.deps.IdentityGrants.Resolve(r.Context(), surface, tokenFromRequest(r))
		if err == nil {
			viewer = v
			break
		}
	}
	// An owner signed in through Plex (or with an owner session) holds a
	// credential identity grants do not resolve; requireAuth admits it via
	// isOwner, so the web player must too, as the owner's own viewer.
	if viewer == nil && h.isOwner(r, tokenFromRequest(r)) && h.deps.Users != nil {
		if users, err := h.deps.Users.Users(r.Context()); err == nil {
			for i := range users {
				if users[i].IsOwner {
					u := users[i]
					viewer = &u
					break
				}
			}
		}
	}
	if viewer == nil {
		writeError(w, 401, "unauthorized", "Sign in required")
		return
	}
	target := r.URL.Query().Get("app")
	if target == "" {
		target = "jellyfin"
	}
	if target != "jellyfin" && target != "vela" {
		writeError(w, 400, "invalid_app", "Unknown web player")
		return
	}
	infos, err := h.deps.Frontends.List(r.Context())
	origin := ""
	if err == nil {
		for _, info := range infos {
			if target == "vela" {
				if info.Descriptor.ID == "vela" && info.Enabled && info.Status.Mounted && info.Status.WebPlayerReady {
					scheme, host, valid := requestOrigin(r, h.proxyPrefixes())
					if valid {
						origin = originURL(scheme, host, "/vela")
					}
				}
				continue
			}

			// The web client downloads on first use: /web answers a
			// self-refreshing "Preparing" page meanwhile, so the button must
			// not wait for an install nobody has triggered yet.
			if info.Descriptor.ID != "jellyfin" || !info.Enabled || !info.Status.Mounted {
				continue
			}
			info = h.presentConnectURLs(r, info)
			// Prefer the browser's current host with the active native listener port.
			scheme, host, valid := requestOrigin(r, h.proxyPrefixes())
			// Native MediaBrowser listeners use HTTP. For a secure console or
			// reverse proxy use its published/shared route instead of guessing TLS.
			if valid && scheme == "http" && !viaProxy(r) {
				hostname := hostOnly(host)
				for _, address := range info.Status.Addresses {
					if _, port, e := net.SplitHostPort(address); e == nil {
						origin = (&url.URL{Scheme: "http", Host: net.JoinHostPort(hostname, port)}).String()
						break
					}
				}
			}
			if origin == "" && h.deps.Config != nil {
				if published, ok := frontendPublicURL(h.deps.Config.Get(), "jellyfin"); ok {
					origin = strings.TrimRight(published, "/")
				}
			}
			if origin == "" && valid {
				origin = originURL(scheme, host, "/jellyfin")
			}

			if origin == "" {
				for _, raw := range info.Status.ConnectURLs {
					u, e := url.Parse(raw)
					if e == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
						origin = strings.TrimRight(raw, "/")
						break
					}
				}
			}
		}
	}
	if origin == "" {
		writeError(w, 503, "unavailable", "Web player unavailable")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]bool{"available": true})
		return
	}
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "Request rejected")
		return
	}
	code, err := h.deps.IdentityGrants.IssueWebCode(r.Context(), viewer.ID)
	if err != nil {
		writeError(w, 503, "unavailable", "Sign-in unavailable")
		return
	}
	path := "/web/ether-signin"
	if target == "vela" {
		path = ""
	}
	writeJSON(w, 200, map[string]string{"url": origin + path + "#code=" + url.QueryEscape(code)})
}
