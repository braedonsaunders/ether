package calendar

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Handler serves GET / (mounted at /ether/api/calendar):
//
//	?from=2026-10-26&to=2026-12-06  dates, to inclusive (default: last week + 6 weeks)
//	&viewer=12                       one viewer (default: the whole household)
//	&types=movie|show                one kind (default: both)
//	&movies=foryou|popular|watchlist|hidden
//	&tz=America/Toronto              which day is today (default UTC)
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query()
		q := Query{Types: p.Get("types"), Movies: p.Get("movies")}
		var bad []string
		date := func(name string) time.Time {
			v := p.Get(name)
			if v == "" {
				return time.Time{}
			}
			t, err := time.Parse(time.DateOnly, v)
			if err != nil {
				bad = append(bad, name)
			}
			return t
		}
		q.From, q.To = date("from"), date("to")
		if v := p.Get("viewer"); v != "" && v != "all" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || id <= 0 {
				bad = append(bad, "viewer")
			}
			q.Viewer = id
		}
		if v := p.Get("tz"); v != "" {
			if loc, err := time.LoadLocation(v); err == nil {
				q.Location = loc
			}
		}
		if len(bad) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "invalid", "message": "invalid " + bad[0], "fields": bad}})
			return
		}
		resp, err := s.Calendar(r.Context(), q)
		if err != nil {
			s.o.Logger.Warn("calendar", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal", "message": "The calendar could not be assembled."}})
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
