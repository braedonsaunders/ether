package migrate

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
)

// Handler is intentionally unauthenticated: mount inside the owner's authenticated router.
func (s *Service) Handler() http.Handler {
	m := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"encode_failed","message":"failed to encode response"}}` + "\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n"))
	}
	reply := func(w http.ResponseWriter, v any, e error) {
		if e != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalid_request", "message": e.Error()}})
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
	replyMissing := func(w http.ResponseWriter) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "preview not found"}})
	}
	// Preview scans the whole library. A large one outlives this POST, so the
	// handler starts a job and the owner polls it. Mount stays owner-only.
	m.HandleFunc("POST /preview", func(w http.ResponseWriter, r *http.Request) {
		var q Request
		e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q)
		var st PreviewStatus
		if e == nil {
			st, e = s.StartPreview(q)
		}
		reply(w, st, e)
	})
	m.HandleFunc("GET /preview/{id}", func(w http.ResponseWriter, r *http.Request) {
		st, ok := s.PreviewStatus(r.PathValue("id"))
		if !ok {
			replyMissing(w)
			return
		}
		reply(w, st, nil)
	})
	m.HandleFunc("POST /preview/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		st, ok := s.CancelPreview(r.PathValue("id"))
		if !ok {
			replyMissing(w)
			return
		}
		reply(w, st, nil)
	})
	m.HandleFunc("POST /import", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			ID string `json:"id"`
		}
		e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q)
		var p Plan
		if e == nil {
			p, e = s.Import(r.Context(), q.ID)
		}
		reply(w, p, e)
	})
	m.HandleFunc("POST /csv/preview", func(w http.ResponseWriter, r *http.Request) {
		viewer, _ := strconv.ParseInt(r.URL.Query().Get("viewer"), 10, 64)
		p, e := s.PreviewCSV(r.Context(), viewer, r.URL.Query().Get("format"), r.URL.Query().Get("kind"), http.MaxBytesReader(w, r.Body, 16<<20))
		reply(w, p, e)
	})
	m.HandleFunc("GET /csv/export", func(w http.ResponseWriter, r *http.Request) {
		viewer, _ := strconv.ParseInt(r.URL.Query().Get("viewer"), 10, 64)
		var buf bytes.Buffer
		e := s.ExportCSV(r.Context(), viewer, r.URL.Query().Get("format"), r.URL.Query().Get("kind"), &buf)
		if e != nil {
			reply(w, nil, e)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="ether-export.csv"`)
		w.Write(buf.Bytes())
	})
	return m
}
