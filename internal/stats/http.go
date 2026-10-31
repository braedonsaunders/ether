package stats

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"time"
)

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]any{"error": map[string]string{"code": "stats", "message": msg}})
}

// Handler must be mounted behind owner authentication with /stats stripped.
func (s *Service) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /sessions/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Playback == nil {
			fail(w, 503, "Playback unavailable")
			return
		}
		_, exists := s.opts.Playback.Session(r.PathValue("id"))
		if s.opts.SessionExists != nil {
			exists = s.opts.SessionExists(r.PathValue("id"))
		}
		if !exists {
			fail(w, 404, "Session not found")
			return
		}
		if s.opts.StopSession != nil {
			if err := s.opts.StopSession(r.PathValue("id")); err != nil {
				fail(w, 500, "Could not stop session")
				return
			}
		} else {
			s.opts.Playback.Stop(r.PathValue("id"))
		}
		reply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		since, until, user := s.period(r)
		p, err := s.History(r.Context(), since, until, user, 500)
		if err != nil {
			fail(w, 500, "History unavailable")
			return
		}
		reply(w, 200, p)
	})
	m.HandleFunc("GET /summary", func(w http.ResponseWriter, r *http.Request) {
		since, until, user := s.period(r)
		v, err := s.Summary(r.Context(), since, until, user)
		if err != nil {
			fail(w, 500, "Summary unavailable")
			return
		}
		reply(w, 200, v)
	})
	m.HandleFunc("GET /infrastructure", s.infrastructure)
	m.HandleFunc("PUT /costs", func(w http.ResponseWriter, r *http.Request) {
		var c Cost
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&c) != nil {
			fail(w, 400, "Invalid cost")
			return
		}
		if err := s.SetCost(r.Context(), c); err != nil {
			fail(w, 400, err.Error())
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /newsletter", s.newsletter)
	m.HandleFunc("GET /newsletter/settings", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.NewsletterSettings(r.Context())
		if err != nil {
			fail(w, 500, "Newsletter settings unavailable")
			return
		}
		reply(w, 200, v)
	})
	m.HandleFunc("PUT /newsletter/settings", func(w http.ResponseWriter, r *http.Request) {
		var v NewsletterSettings
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&v) != nil {
			fail(w, 400, "Invalid newsletter settings")
			return
		}
		if err := s.SaveNewsletter(r.Context(), v); err != nil {
			fail(w, 400, err.Error())
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /push", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"supported": false, "reason": "Web Push delivery is not configured. Use a notification channel for background alerts."})
	})
	return m
}
func (s *Service) period(r *http.Request) (time.Time, time.Time, int64) {
	now := s.opts.Now().UTC()
	until := now.Add(time.Second)
	since := now.AddDate(0, 0, -30)
	if year, err := strconv.Atoi(r.URL.Query().Get("year")); err == nil && year >= 2000 && year <= now.Year() {
		since = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		until = since.AddDate(1, 0, 0)
	} else if days, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && days > 0 && days <= 3660 {
		since = now.AddDate(0, 0, -days)
	}
	user, _ := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	return since, until, user
}
func (s *Service) infrastructure(w http.ResponseWriter, r *http.Request) {
	type Snapshot struct {
		Data json.RawMessage `json:"data"`
		At   int64           `json:"at"`
	}
	snapshots := map[string]Snapshot{}
	for _, kind := range []string{"providers", "indexers"} {
		var raw string
		var at int64
		if s.db.QueryRowContext(r.Context(), `SELECT payload,at FROM stats_snapshots WHERE kind=?`, kind).Scan(&raw, &at) == nil {
			snapshots[kind] = Snapshot{json.RawMessage(raw), at}
		}
	}
	type Usage struct {
		ID            string `json:"id"`
		Bytes         int64  `json:"bytes"`
		Articles      int64  `json:"articles"`
		Misses        int64  `json:"misses"`
		Errors        int64  `json:"errors"`
		BodyMisses    int64  `json:"bodyMisses"`
		BodyTakedowns int64  `json:"bodyTakedowns"`
		BackfillSaves int64  `json:"backfillSaves"`
		Spillovers    int64  `json:"spillovers"`
	}
	usage := []Usage{}
	month := s.opts.Now().UTC().Format("2006-01") + "-01"
	rows, err := s.db.QueryContext(r.Context(), `SELECT provider,CAST(SUM(bytes) AS BIGINT),CAST(SUM(articles) AS BIGINT),CAST(SUM(misses) AS BIGINT),CAST(SUM(errors) AS BIGINT),CAST(SUM(backfill_saves) AS BIGINT),CAST(SUM(body_misses) AS BIGINT),CAST(SUM(body_takedowns) AS BIGINT),CAST(SUM(spillovers) AS BIGINT) FROM stats_provider_days WHERE day>=? GROUP BY provider`, month)
	if err != nil {
		fail(w, 500, "Usage unavailable")
		return
	}
	for rows.Next() {
		var u Usage
		if rows.Scan(&u.ID, &u.Bytes, &u.Articles, &u.Misses, &u.Errors, &u.BackfillSaves, &u.BodyMisses, &u.BodyTakedowns, &u.Spillovers) != nil {
			rows.Close()
			fail(w, 500, "Usage unavailable")
			return
		}
		usage = append(usage, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 500, "Usage unavailable")
		return
	}
	costs := []Cost{}
	rows, err = s.db.QueryContext(r.Context(), `SELECT kind,id,monthly,currency FROM stats_costs ORDER BY kind,id`)
	if err != nil {
		fail(w, 500, "Costs unavailable")
		return
	}
	for rows.Next() {
		var c Cost
		if rows.Scan(&c.Kind, &c.ID, &c.Monthly, &c.Currency) != nil {
			rows.Close()
			fail(w, 500, "Costs unavailable")
			return
		}
		costs = append(costs, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 500, "Costs unavailable")
		return
	}
	indexerMonth, err := s.IndexerMonth(r.Context())
	if err != nil {
		fail(w, 500, "Indexer metrics unavailable")
		return
	}
	var monthPlays, failovers, failedPlays int64
	monthStart, _ := time.Parse("2006-01-02", month)
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*), CAST(COALESCE(SUM(failovers),0) AS BIGINT), CAST(COALESCE(SUM(CASE WHEN error<>'' THEN 1 ELSE 0 END),0) AS BIGINT) FROM stats_plays WHERE started>=?`, monthStart.Unix()).Scan(&monthPlays, &failovers, &failedPlays); err != nil {
		fail(w, 500, "Monthly play count unavailable")
		return
	}
	type ProviderDay struct {
		ID       string `json:"id"`
		Day      string `json:"day"`
		Bytes    int64  `json:"bytes"`
		Articles int64  `json:"articles"`
		Misses   int64  `json:"misses"`
		Errors   int64  `json:"errors"`
	}
	providerDays := []ProviderDay{}
	rows, err = s.db.QueryContext(r.Context(), `SELECT provider,day,bytes,articles,misses,errors FROM stats_provider_days WHERE day>=? ORDER BY day,provider`, month)
	if err != nil {
		fail(w, 500, "Provider history unavailable")
		return
	}
	for rows.Next() {
		var d ProviderDay
		if rows.Scan(&d.ID, &d.Day, &d.Bytes, &d.Articles, &d.Misses, &d.Errors) != nil {
			rows.Close()
			fail(w, 500, "Provider history unavailable")
			return
		}
		providerDays = append(providerDays, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 500, "Provider history unavailable")
		return
	}
	type IndexerDay struct {
		Indexer   string `json:"indexer"`
		Day       string `json:"day"`
		Searches  int64  `json:"searches"`
		Successes int64  `json:"successes"`
		Grabs     int64  `json:"grabs"`
		LatencyMs int64  `json:"latencyMs"`
	}
	indexerDays := []IndexerDay{}
	rows, err = s.db.QueryContext(r.Context(), `SELECT indexer,day,searches,successes,grabs,latency_ms FROM stats_indexer_days WHERE day>=? ORDER BY day,indexer`, month)
	if err != nil {
		fail(w, 500, "Indexer history unavailable")
		return
	}
	for rows.Next() {
		var d IndexerDay
		if rows.Scan(&d.Indexer, &d.Day, &d.Searches, &d.Successes, &d.Grabs, &d.LatencyMs) != nil {
			rows.Close()
			fail(w, 500, "Indexer history unavailable")
			return
		}
		indexerDays = append(indexerDays, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		fail(w, 500, "Indexer history unavailable")
		return
	}
	reply(w, 200, map[string]any{
		"monthPlays": monthPlays, "failovers": failovers, "failedPlays": failedPlays,
		"snapshots": snapshots, "providerMonth": usage, "providerDays": providerDays,
		"indexerDays": indexerDays, "costs": costs, "indexerMonth": indexerMonth, "month": month,
		"notes": []string{"Provider totals accumulate observed counters; daily boundaries use sample time.", "Latency is the provider's live EWMA. Article success is not title completion.", "Plays are counted for the server, not split by source. Cache hits are not stored here."},
	})
}

var newsletterTemplate = template.Must(template.New("newsletter").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Ether viewing report</title></head><body style="font:16px system-ui;max-width:640px;margin:32px auto;color:#222"><h1>Your Ether week</h1><p>{{.Since.Format "Jan 2, 2006"}} – {{.Until.Format "Jan 2, 2006"}} · UTC</p><p>{{.Plays}} plays · {{.Completed}} completed · {{.Hours}} hours of reported playback</p><h2>Popular on your server</h2>{{if .Titles}}<ol>{{range .Titles}}<li>{{.Label}} — {{.Plays}} plays</li>{{end}}</ol>{{else}}<p>No recorded plays this week.</p>{{end}}<p style="color:#666">Playback time comes from client timeline updates; gaps and ambiguous simultaneous sessions are excluded.</p></body></html>`))

func (s *Service) newsletter(w http.ResponseWriter, r *http.Request) {
	user, _ := strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
	html, err := s.RenderNewsletter(r.Context(), user)
	if err != nil {
		fail(w, 500, "Newsletter unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}
