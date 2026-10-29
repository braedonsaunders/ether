package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	qrcode "github.com/skip2/go-qrcode"
)

func (h *Handler) portalConnectApps(w http.ResponseWriter, r *http.Request) {
	viewer, err := h.portalViewer(r)
	if err != nil || viewer == nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	h.connectApps(w, r, viewer)
}

func (h *Handler) ownerConnectApps(w http.ResponseWriter, r *http.Request) {
	access, ok := RequestAccess(r.Context())
	if !ok || !access.Owner || !h.isOwner(r, tokenFromRequest(r)) || h.deps.ActiveViewer == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid", "invalid viewer")
		return
	}
	viewer, err := h.deps.ActiveViewer(r.Context(), id)
	if err != nil || viewer == nil {
		writeError(w, 404, "not_found", "viewer unavailable")
		return
	}
	h.connectApps(w, r, viewer)
}

func (h *Handler) connectApps(w http.ResponseWriter, r *http.Request, viewer *core.Viewer) {
	w.Header().Set("Cache-Control", "no-store")
	if h.deps.Frontends == nil || h.deps.IdentityGrants == nil {
		writeError(w, 503, "unavailable", "apps unavailable")
		return
	}
	infos, err := h.deps.Frontends.List(r.Context())
	if err != nil {
		writeError(w, 503, "unavailable", "apps unavailable")
		return
	}
	enabled := map[string]bool{}
	for _, info := range infos {
		id := string(info.Descriptor.ID)
		if info.Enabled && (id == "stremio" || id == "webdav" || id == "m3u") {
			enabled[id] = true
		}
	}
	if r.Method == http.MethodGet {
		out := make([]string, 0, len(enabled))
		for _, id := range []string{"stremio", "webdav", "m3u"} {
			if enabled[id] {
				out = append(out, id)
			}
		}
		writeJSON(w, 200, map[string]any{"apps": out})
		return
	}
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	surface := r.PathValue("surface")
	if !enabled[surface] {
		writeError(w, 404, "not_found", "app unavailable")
		return
	}
	if h.deps.ViewerPortalURL == nil {
		writeError(w, 503, "unavailable", "address unavailable")
		return
	}
	portal, err := h.deps.ViewerPortalURL(r.Context(), r)
	u, parseErr := url.Parse(portal)
	if err != nil || parseErr != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !strings.HasSuffix(u.Path, "/ether/me") {
		writeError(w, 503, "unavailable", "address unavailable")
		return
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		writeError(w, 503, "unavailable", "app unavailable")
		return
	}
	device := core.Device{ID: surface + "-" + hex.EncodeToString(id[:]), Client: surface, Product: surface}
	secret, err := h.deps.IdentityGrants.Issue(r.Context(), viewer.ID, nil, surface, device)
	if err != nil {
		writeError(w, 404, "not_found", "viewer unavailable")
		return
	}
	prefix := strings.TrimSuffix(u.Path, "/ether/me")
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	result := map[string]string{"surface": surface}
	switch surface {
	case "stremio":
		u.Path = prefix + "/stremio/" + secret + "/manifest.json"
		result["url"] = u.String()
	case "m3u":
		u.Path = prefix + "/m3u/" + secret + "/playlist.m3u"
		result["url"] = u.String()
	case "webdav":
		u.Path = prefix + "/webdav/"
		result["url"] = u.String()
		result["username"] = strconv.FormatInt(viewer.ID, 10)
		result["password"] = secret
	}
	qrValue := result["url"]
	if surface == "webdav" {
		qrURL := *u
		qrURL.User = url.UserPassword(result["username"], secret)
		qrValue = qrURL.String()
	}
	png, err := qrcode.Encode(qrValue, qrcode.Medium, 256)
	if err != nil {
		hash := sha256.Sum256([]byte(secret))
		_ = h.deps.IdentityGrants.RevokeGrant(r.Context(), viewer.ID, hex.EncodeToString(hash[:]))
		writeError(w, 503, "unavailable", "could not create connection code")
		return
	}
	result["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	writeJSON(w, 200, result)
}
