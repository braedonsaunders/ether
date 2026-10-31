package app

import (
	"context"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/store"
)

// databaseHealth is shared-database connectivity as the owner UI shows
// it: GET /ether/api/cluster/ (field "database") and
// GET /ether/api/status/database.
type databaseHealth struct {
	// Clustered is false on a standalone (SQLite) server, which reports
	// nothing else.
	Clustered bool `json:"clustered"`
	// State is "ok", "degraded" (unreachable; writes are being held) or
	// "recovering" (reachable again; held writes are being written).
	State     string     `json:"state"`
	Reachable bool       `json:"reachable"`
	DownSince *time.Time `json:"downSince,omitempty"`
	LastError string     `json:"lastError,omitempty"`
	// LastHeartbeat is this node's last successful cluster heartbeat.
	LastHeartbeat *time.Time `json:"lastHeartbeat,omitempty"`
	// Held is the number of writes (watch progress, resume points, play
	// records) kept in memory until the database answers; Dropped counts
	// writes discarded because an outbox was full.
	Held     int                  `json:"held"`
	Dropped  int64                `json:"dropped,omitempty"`
	Outboxes []store.OutboxStatus `json:"outboxes,omitempty"`
	// Message is a one-line summary for the owner.
	Message string `json:"message,omitempty"`
	// Integrity is the last check of a SQLite database for damage (at
	// start and in daily maintenance); absent until the first one ran.
	Integrity *store.Integrity `json:"integrity,omitempty"`
}

func (a *App) databaseHealth() databaseHealth {
	if !a.clustered() {
		return databaseHealth{State: "ok", Reachable: true, Integrity: a.integrity.Load()}
	}
	h := databaseHealth{Clustered: true}
	av := a.dbAvail.State()
	h.Reachable, h.LastError = av.Reachable, av.LastError
	if !av.DownSince.IsZero() {
		t := av.DownSince
		h.DownSince = &t
	}
	if pg, ok := a.cluster.(*cluster.Postgres); ok {
		if t := pg.Health().LastHeartbeat; !t.IsZero() {
			h.LastHeartbeat = &t
		}
	}
	add := func(st store.OutboxStatus, ok bool) {
		if !ok {
			return
		}
		h.Outboxes = append(h.Outboxes, st)
		h.Held += st.Depth
		h.Dropped += st.Dropped
	}
	if a.state != nil {
		add(a.state.OutboxStatus())
	}
	if a.playback != nil {
		add(a.playback.ResumeOutboxStatus())
	}
	if a.stats != nil {
		add(a.stats.OutboxStatus())
	}
	switch {
	case !h.Reachable:
		h.State = "degraded"
		h.Message = fmt.Sprintf("Database unreachable since %s — active streams continue and progress is being held (%d pending).",
			h.DownSince.Local().Format("15:04:05"), h.Held)
	case h.Held > 0:
		h.State = "recovering"
		h.Message = fmt.Sprintf("Database reachable again — writing %d held updates.", h.Held)
	default:
		h.State = "ok"
	}
	return h
}

func (a *App) databaseStatus(context.Context) any { return a.databaseHealth() }
