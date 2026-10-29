package stremio

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) setup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&input); err != nil || input.Username == "" || input.Password == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	base := i.imageBase(r)
	if base == nil || i.host.ClientIP == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	ip, ok := i.host.ClientIP(r)
	if !ok {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	device := frontend.Device{ID: "stremio-" + hex.EncodeToString(id[:]), Client: "Stremio", Product: "Stremio"}
	credential, err := i.host.Identity.SignIn(frontend.WithClientIP(r.Context(), ip), input.Username, input.Password, device)
	if err != nil {
		if errors.Is(err, frontend.ErrRateLimited) {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		} else {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		}
		return
	}
	base.Path = "/stremio/" + url.PathEscape(credential) + "/manifest.json"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"url": base.String()})
}
