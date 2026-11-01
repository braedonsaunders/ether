package clusterconfig

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// HandlerOptions configure the owner API.
type HandlerOptions struct {
	// DB is the cluster database; nil or SQLite means clustering is off.
	DB *sql.DB
	// Clustered reports whether this node runs against PostgreSQL.
	Clustered bool
	Config    *config.Store
	// DatabaseURL is the URL placed in new tokens (the node's own).
	DatabaseURL func() string
}

// Handler is the owner API mounted at /ether/api/cluster:
//
//	GET    /settings             shared-settings status and join tokens
//	POST   /join-tokens          create a one-time join token {ttlMinutes?, databaseUrl?}
//	DELETE /join-tokens/{id}     revoke an unused token
//
// The admin package authenticates the owner; token creation additionally
// requires setup to be complete, so the unauthenticated LAN access the
// wizard allows before then can never mint a token.
func Handler(o HandlerOptions) http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"encode_failed","message":"failed to encode response"}}` + "\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n"))
	}
	writeError := func(w http.ResponseWriter, code int, kind, msg string) {
		writeJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}})
	}
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"clustered": o.Clustered && o.Config.Clustered(), "keyFingerprint": o.Config.KeyFingerprint()[:16]}
		if o.Clustered {
			out["sharedVersion"] = o.Config.SharedVersion()
			tokens, err := ListJoinTokens(r.Context(), o.DB)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "database", err.Error())
				return
			}
			out["joinTokens"] = tokens
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /join-tokens", func(w http.ResponseWriter, r *http.Request) {
		if !o.Clustered || !o.Config.Clustered() {
			writeError(w, http.StatusConflict, "not_clustered", "Create a cluster from the Cluster page first.")
			return
		}
		if !o.Config.Get().SetupComplete {
			writeError(w, http.StatusForbidden, "setup_incomplete", "finish setup before adding nodes")
			return
		}
		var body struct {
			TTLMinutes  int    `json:"ttlMinutes"`
			DatabaseURL string `json:"databaseUrl"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
				return
			}
		}
		ttl := DefaultJoinTokenTTL
		if body.TTLMinutes > 0 && body.TTLMinutes < 60 {
			ttl = time.Duration(body.TTLMinutes) * time.Minute
		}
		url := body.DatabaseURL
		if url == "" && o.DatabaseURL != nil {
			url = o.DatabaseURL()
		}
		if !isPostgresURL(url) {
			writeError(w, http.StatusBadRequest, "bad_request", "databaseUrl must be a postgres:// URL")
			return
		}
		tok, info, err := NewToken(r.Context(), o.DB, o.Config, url, ttl)
		if err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, config.ErrKeyMismatch) {
				code = http.StatusConflict
			}
			writeError(w, code, "token", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":     tok.String(),
			"id":        info.ID,
			"expiresAt": info.ExpiresAt,
			"command":   "ether cluster join <token>",
			"warning":   TokenWarning,
		})
	})
	mux.HandleFunc("DELETE /join-tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !o.Clustered {
			writeError(w, http.StatusConflict, "not_clustered", "clustering is not enabled")
			return
		}
		ok, err := RevokeJoinToken(r.Context(), o.DB, r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "database", err.Error())
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no unused token with that id")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
