package admin

import (
	"net/http"
	"net/url"

	qrcode "github.com/skip2/go-qrcode"
)

// frontendQR encodes an owner-supplied connection URL locally; it never
// contacts the target or sends addresses to a third-party QR service.
func (h *Handler) frontendQR(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 1024 || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		writeError(w, http.StatusBadRequest, "invalid_url", "A connection URL is required")
		return
	}
	png, err := qrcode.Encode(raw, qrcode.Medium, 256)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_url", "Could not encode the connection URL")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}
