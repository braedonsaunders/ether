package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func reply(w http.ResponseWriter, status int, v any) {
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
	w.WriteHeader(status)
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n"))
}
func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]any{"error": map[string]string{"code": "notifications", "message": msg}})
}

// Handler is an owner-only API; mount with /notifications stripped.
func (s *Service) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		cs, err := s.Channels(r.Context())
		if err != nil {
			fail(w, 500, "Channels unavailable")
			return
		}
		type Delivery struct {
			ID        int64  `json:"id"`
			Channel   string `json:"channel"`
			Created   int64  `json:"created"`
			Status    string `json:"status"`
			Error     string `json:"error"`
			Kind      string `json:"kind"`
			Attempted int64  `json:"attempted"`
			Due       int64  `json:"due"`
		}
		deliveries := []Delivery{}
		rows, err := s.db.QueryContext(r.Context(), `SELECT id,channel,created,status,error,event,attempted,due FROM notify_queue ORDER BY id DESC LIMIT 100`)
		if err != nil {
			fail(w, 500, "Delivery log unavailable")
			return
		}
		for rows.Next() {
			var d Delivery
			var raw string
			if rows.Scan(&d.ID, &d.Channel, &d.Created, &d.Status, &d.Error, &raw, &d.Attempted, &d.Due) != nil {
				rows.Close()
				fail(w, 500, "Delivery log unavailable")
				return
			}
			var e Event
			_ = json.Unmarshal([]byte(raw), &e)
			d.Kind = e.Kind
			deliveries = append(deliveries, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			fail(w, 500, "Delivery log unavailable")
			return
		}
		reply(w, 200, map[string]any{"channels": cs, "events": Kinds, "deliveries": deliveries, "eventCatalog": eventCatalog(), "crashWatcher": s.monitorActive()})
	})
	m.HandleFunc("PUT /{id}", func(w http.ResponseWriter, r *http.Request) {
		var c Channel
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&c) != nil {
			fail(w, 400, "Invalid channel JSON")
			return
		}
		c.ID = r.PathValue("id")
		if err := s.Save(r.Context(), c); err != nil {
			fail(w, 400, err.Error())
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("DELETE /{id}", func(w http.ResponseWriter, r *http.Request) {
		tx, err := s.db.BeginTx(r.Context(), nil)
		if err != nil {
			fail(w, 500, "Delete unavailable")
			return
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(r.Context(), `DELETE FROM notify_channels WHERE id=?`, r.PathValue("id")); err == nil {
			_, err = tx.ExecContext(r.Context(), `UPDATE notify_queue SET status='cancelled',error='Channel deleted' WHERE channel=? AND status='pending'`, r.PathValue("id"))
		}
		if err != nil || tx.Commit() != nil {
			fail(w, 500, "Delete unavailable")
			return
		}
		s.refreshMonitor()
		reply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("POST /{id}/test", func(w http.ResponseWriter, r *http.Request) {
		cs, err := s.channels(r.Context())
		if err != nil {
			fail(w, 500, "Channels unavailable")
			return
		}
		for _, c := range cs {
			if c.ID == r.PathValue("id") {
				ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
				defer cancel()
				e := Event{Kind: "test", Title: "Ether notification test", Message: "Your notification channel is connected. Real alerts will use your selected event rules.", At: s.opts.Now()}
				id, err := s.recordDelivery(ctx, c, e)
				if err != nil {
					fail(w, 500, "Could not record test")
					return
				}
				status, detail := "sent", ""
				if err := s.deliver(ctx, c, e); err != nil {
					status, detail = "failed", err.Error()
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE notify_queue SET status=?,error=? WHERE id=?`, status, detail, id); err != nil {
					fail(w, 500, "Test finished but its result could not be recorded")
					return
				}
				if status == "failed" {
					fail(w, http.StatusBadGateway, detail)
					return
				}
				reply(w, http.StatusOK, map[string]any{"sent": true, "id": id})
				return
			}
		}
		fail(w, 404, "Channel not found")
	})
	m.HandleFunc("GET /heartbeat", func(w http.ResponseWriter, r *http.Request) {
		h, err := s.HeartbeatSettings(r.Context(), true)
		if err != nil {
			fail(w, 500, "Heartbeat settings unavailable")
			return
		}
		reply(w, 200, h)
	})
	m.HandleFunc("PUT /heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var h Heartbeat
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&h) != nil {
			fail(w, 400, "Invalid heartbeat settings")
			return
		}
		if err := s.SaveHeartbeat(r.Context(), h); err != nil {
			fail(w, 400, err.Error())
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
	})
	m.HandleFunc("POST /heartbeat/test", func(w http.ResponseWriter, r *http.Request) {
		h, err := s.HeartbeatSettings(r.Context(), false)
		if err != nil || h.Endpoint == "" {
			fail(w, 400, "Save a heartbeat URL first")
			return
		}
		if err := s.pingHeartbeat(r.Context(), h); err != nil {
			fail(w, 502, err.Error())
			return
		}
		reply(w, 200, map[string]bool{"sent": true})
	})
	m.HandleFunc("GET /push/key", func(w http.ResponseWriter, r *http.Request) {
		_, public, err := s.pushKeys(r.Context())
		if err != nil {
			fail(w, 500, "Push identity unavailable")
			return
		}
		reply(w, 200, map[string]string{"publicKey": public})
	})
	m.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) {
		var e Event
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&e) != nil {
			fail(w, 400, "Invalid event")
			return
		}
		e.At = time.Time{}
		if err := s.Emit(r.Context(), e); err != nil {
			fail(w, 400, err.Error())
			return
		}
		reply(w, 202, map[string]bool{"queued": true})
	})
	return m
}

func (s *Service) recordDelivery(ctx context.Context, c Channel, e Event) (int64, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	var id int64
	now := s.opts.Now().Unix()
	err = s.db.QueryRowContext(ctx, `INSERT INTO notify_queue(channel,event,created,due,status,attempted,claimed_by,claimed_at) VALUES(?,?,?,?,'sending',?,?,?) RETURNING id`, c.ID, string(raw), now, now, now, s.opts.NodeID, now).Scan(&id)
	return id, err
}
