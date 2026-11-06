package onair

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/fonts"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"net/http"
)

// MCP exclusion: these owner-authenticated routes supply immutable rendering
// resources to the console. They do not expose owner records or mutations;
// fetching binary fonts and bundled scene data has no useful agent tool analogue.
func ServeFont(w http.ResponseWriter, r *http.Request) {
	data := fonts.Bytes(r.PathValue("id"))
	tag := fmt.Sprintf(`"%x"`, sha256.Sum256(data))
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "font/ttf")
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}
func ServeSystemScreens(w http.ResponseWriter, r *http.Request) {
	out := map[string]scene.Document{}
	for _, moment := range families.Names()["signal"] {
		d, err := families.Get("signal", moment)
		if err != nil {
			http.Error(w, "invalid system screen", 500)
			return
		}
		out[moment] = d
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
