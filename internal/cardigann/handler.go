package cardigann

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// HandlerDeps are the owner API's links to configuration.
type HandlerDeps struct {
	// Indexers returns the configured indexers (to refuse removing a
	// definition that is in use).
	Indexers func() []config.Indexer
	// Unredact restores secrets the UI sent back as placeholders.
	Unredact func(config.Indexer) config.Indexer
}

// Handler serves the owner API for built-in indexers:
//
//	GET    /definitions            installed definitions
//	POST   /definitions            install an uploaded definition {"yaml": "..."}
//	GET    /definitions/{id}       one installed definition
//	DELETE /definitions/{id}       remove a definition no indexer uses
//	GET    /catalog[?refresh=1]    the definitions source listing
//	POST   /catalog/install        install {"id": "..."} from the source
//	POST   /update                 refresh definitions installed from the source
//	POST   /test                   test an indexer config (saved or not)
//	GET    /status                 last test result per indexer
func (s *Service) Handler(deps HandlerDeps) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /definitions", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Definitions(r.Context())
		if err != nil {
			s.log.Error("cardigann: list definitions", "err", err)
			writeErr(w, http.StatusInternalServerError, "internal", "could not list definitions")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definitions": list})
	})
	m.HandleFunc("POST /definitions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			YAML string `json:"yaml"`
		}
		if !decode(w, r, &in, maxDefinitionBytes+64<<10) {
			return
		}
		sum, err := s.Install(r.Context(), []byte(in.YAML), "", "")
		if err != nil {
			writeDefinitionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definition": sum})
	})
	m.HandleFunc("GET /definitions/{id}", func(w http.ResponseWriter, r *http.Request) {
		sum, err := s.Definition(r.Context(), r.PathValue("id"))
		if err != nil {
			writeDefinitionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definition": sum})
	})
	m.HandleFunc("DELETE /definitions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if deps.Indexers != nil {
			var users []string
			for _, ix := range deps.Indexers() {
				if ix.IsBuiltin() && ix.Definition == id {
					users = append(users, ix.Name)
				}
			}
			if len(users) > 0 {
				writeErr(w, http.StatusConflict, "in_use", "remove the indexers that use this definition first: "+strings.Join(users, ", "))
				return
			}
		}
		if err := s.Remove(r.Context(), id); err != nil {
			writeDefinitionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": id})
	})
	m.HandleFunc("GET /catalog", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		list, err := s.Catalog(ctx, r.URL.Query().Get("refresh") == "1")
		if err != nil {
			writeDefinitionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": list})
	})
	m.HandleFunc("POST /catalog/install", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &in, 4<<10) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		sum, err := s.InstallFromSource(ctx, in.ID)
		if err != nil {
			writeDefinitionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definition": sum})
	})
	m.HandleFunc("POST /update", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		rep, err := s.Update(ctx)
		if err != nil {
			writeDefinitionError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	m.HandleFunc("POST /test", func(w http.ResponseWriter, r *http.Request) {
		var ix config.Indexer
		if !decode(w, r, &ix, 256<<10) {
			return
		}
		if deps.Unredact != nil {
			ix = deps.Unredact(ix)
		}
		ix.Enabled = true
		saved := false
		if deps.Indexers != nil && ix.ID != "" {
			for _, c := range deps.Indexers() {
				saved = saved || c.ID == ix.ID
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		start := time.Now()
		res := s.Test(ctx, ix, saved)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": res.OK, "results": res.Results, "titles": res.Titles, "error": res.Error,
			"durationMs": time.Since(start).Milliseconds(),
		})
	})
	m.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Statuses(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "could not read test results")
			return
		}
		if list == nil {
			list = []TestStatus{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"statuses": list})
	})
	return m
}

func decode(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body is too large")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "invalid request body")
		return false
	}
	return true
}

func writeDefinitionError(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "invalid_definition", "message": err.Error(), "problems": ve.Problems}})
	case errors.Is(err, ErrUnknownDefinition):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrNoSource):
		writeErr(w, http.StatusConflict, "no_source", "set a definitions source URL first")
	default:
		writeErr(w, http.StatusBadGateway, "definition_error", err.Error())
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// writeJSON encodes before writing the status, so an unencodable value is a
// 500, never an empty 200.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("encode response", "err", err)
		b, status = []byte(`{"error":{"code":"encode_failed","message":"the server could not encode this response"}}`), http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
