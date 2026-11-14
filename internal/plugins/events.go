package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func newEventID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Publish normalizes and fans out one Ether event: it is written to the
// 7-day event log and queued for every enabled plugin that subscribed to
// it (and was not revoked). Viewer identity is stripped per plugin unless
// the plugin's grants allow it.
func (m *Manager) Publish(ctx context.Context, ev Event) {
	if !KnownEvents[ev.Name] {
		return
	}
	m.mu.Lock()
	recs := make([]Record, 0, len(m.eventSubs[ev.Name]))
	for _, r := range m.eventSubs[ev.Name] {
		recs = append(recs, r)
	}
	m.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	if ev.ID == "" {
		ev.ID = newEventID()
	}
	if ev.At.IsZero() {
		ev.At = m.now()
	}
	logged := false
	for _, r := range recs {
		if !r.Enabled || r.Manifest.Ether == nil || !r.Manifest.HasCapability(CapEvents) {
			continue
		}
		sub := false
		if evMeta := r.Manifest.Ether.Contributes.Events; evMeta != nil {
			for _, n := range evMeta.Subscribe {
				if n == ev.Name {
					sub = true
				}
			}
		}
		if !sub {
			continue
		}
		g, err := m.store.grants(ctx, r.ID)
		if err != nil || g.revoked("events") {
			continue
		}
		if !logged {
			m.store.logEvent(ctx, ev)
			logged = true
		}
		out := ev
		if !viewerOK(g, normLevel(r.Manifest.Ether.Permissions.Viewers.Identity), ev.ViewerID) {
			out.ViewerID = 0
			out.Payload = stripViewer(out.Payload)
		}
		dropped, err := m.store.enqueue(ctx, r.ID, out)
		if err == nil && dropped > 0 {
			m.healthMu.Lock()
			if h, err := m.store.health(ctx, r.ID); err == nil {
				h.Drops += int64(dropped)
				_ = m.store.putHealth(ctx, r.ID, h)
			}
			m.healthMu.Unlock()
		}
	}
}

// indexEventSubscriptions is updated together with the plugin's descriptors.
func (m *Manager) indexEventSubscriptions(r Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, subscribers := range m.eventSubs {
		delete(subscribers, r.ID)
	}
	if !r.Enabled || r.Manifest.Ether == nil || !r.Manifest.HasCapability(CapEvents) || r.Manifest.Ether.Contributes.Events == nil {
		return
	}
	for _, name := range r.Manifest.Ether.Contributes.Events.Subscribe {
		if !KnownEvents[name] {
			continue
		}
		if m.eventSubs[name] == nil {
			m.eventSubs[name] = make(map[string]Record)
		}
		m.eventSubs[name][r.ID] = r
	}
}

func stripViewer(payload json.RawMessage) json.RawMessage {
	var doc any
	if json.Unmarshal(payload, &doc) != nil {
		return json.RawMessage(`{}`)
	}
	changed := false
	var strip func(any)
	strip = func(v any) {
		switch value := v.(type) {
		case map[string]any:
			for k, child := range value {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "viewer") || strings.Contains(lk, "user") {
					delete(value, k)
					changed = true
				} else {
					strip(child)
				}
			}
		case []any:
			for _, child := range value {
				strip(child)
			}
		}
	}
	strip(doc)
	if !changed {
		return payload
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return out
}

// NormalizePlayback maps a playback.Manager event kind to §5.7 names.
// state is the raw timeline state for updates (playing, paused, stopped).
// It returns "" for kinds with no normalized event.
func NormalizePlayback(kind, state string) string {
	switch kind {
	case "start", "started":
		return EventPlaybackStarted
	case "stop", "stopped":
		return EventPlaybackStopped
	case "completed", "complete":
		return EventPlaybackCompleted
	case "error":
		return EventPlaybackError
	case "failover", "downfailover":
		return EventPlaybackFailover
	case "update", "progress":
		switch state {
		case "paused":
			return EventPlaybackPaused
		case "playing":
			return EventPlaybackResumed
		}
	}
	return ""
}

// NormalizeNotify maps a notify event kind to §5.7 names.
func NormalizeNotify(kind string) string {
	switch kind {
	case "title.available":
		return EventTitleAvailable
	case "user.joined":
		return EventViewerAdded
	case "playback.error":
		return EventPlaybackError
	case "playback.failover":
		return EventPlaybackFailover
	case "playback.start":
		return EventPlaybackStarted
	case "playback.stop":
		return EventPlaybackStopped
	}
	return ""
}

// DeliverOutbox sends queued events to one plugin in batches of 50 and
// deletes the ones it acks. A missing ack keeps events for redelivery
// (at least once).
func (m *Manager) DeliverOutbox(ctx context.Context, pluginID string) error {
	for {
		evs, ids, err := m.store.outboxBatch(ctx, pluginID, 50)
		if err != nil || len(evs) == 0 {
			return err
		}
		body, _ := json.Marshal(map[string]any{"events": evs})
		resp, err := m.Call(ctx, pluginID, Request{Method: "POST", Path: "/ether/event", Body: body})
		if err != nil {
			return err
		}
		if resp.Status < 200 || resp.Status >= 300 {
			return fmt.Errorf("event delivery failed with status %d", resp.Status)
		}
		var out struct {
			Ack []string `json:"ack"`
		}
		_ = json.Unmarshal(resp.Body, &out)
		acked := map[string]bool{}
		for _, id := range out.Ack {
			acked[id] = true
		}
		var done []int64
		for i, ev := range evs {
			if acked[ev.ID] {
				done = append(done, ids[i])
			}
		}
		if len(done) == 0 {
			return nil // nothing acked; try again later
		}
		if err := m.store.ackOutbox(ctx, done); err != nil {
			return err
		}
		if len(done) < len(evs) {
			return nil
		}
	}
}

// Run drives health checks, scheduled jobs and outbox delivery until ctx
// ends. Leader plugins run jobs and event delivery only on the leader;
// every_node plugins run everywhere. Health checks run on every node.
func (m *Manager) Run(ctx context.Context) {
	defer m.cancelActionJobs("")
	defer func() {
		m.hashMu.Lock()
		defer m.hashMu.Unlock()
		for _, job := range m.hashJobs {
			job.cancel()
		}
	}()

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	lastHealth := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.runOnce(ctx, lastHealth)
		}
	}
}

// runOnce is one scheduler pass; tests drive it directly.
// stremioManifestRefresh is how often an installed Stremio add-on's
// manifest is re-read.
const stremioManifestRefresh = time.Hour

func (m *Manager) runOnce(ctx context.Context, lastHealth map[string]time.Time) {
	recs, err := m.store.list(ctx)
	if err != nil {
		return
	}
	now := m.now()
	m.refreshOnAir(ctx, recs, now)
	for _, r := range recs {
		if r.Enabled && r.Manifest.IsStremioOnly() && r.SourceKind == "url" && m.isLeader() {
			// Stremio add-ons change catalogs and resources without a
			// version bump; re-read the manifest hourly (cluster leader).
			key := "stremio-manifest|" + r.ID
			if last, seen := lastHealth[key]; !seen || now.Sub(last) >= stremioManifestRefresh {
				lastHealth[key] = now
				if seen {
					go func(id string) {
						rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
						defer cancel()
						_, _, _ = m.CheckUpdate(rctx, id)
					}(r.ID)
				}
			}
		}
		if !r.Enabled || r.Manifest.Ether == nil {
			continue
		}
		interval := time.Minute
		if d, err := time.ParseDuration(r.Manifest.Ether.Health.Interval); err == nil && d >= 15*time.Second {
			interval = d
		}
		if _, hasRuntime := m.factoryFor(r); hasRuntime && now.Sub(lastHealth[r.ID]) >= interval {
			lastHealth[r.ID] = now
			h, _ := m.store.health(ctx, r.ID)
			if h.NextRetry.IsZero() || !now.Before(h.NextRetry) || h.Status != "error" {
				_, _ = m.CheckHealth(ctx, r.ID)
			}
		}
		if !m.mayRunJobs(r) {
			continue
		}
		for _, job := range r.Manifest.Ether.Schedule {
			if job.Call == "/ether/feeds" {
				continue
			}
			every, err := time.ParseDuration(job.Every)
			if err != nil {
				continue
			}
			key := r.ID + "|" + job.Call
			m.mu.Lock()
			next, seen := m.nextRun[key]
			if !seen {
				m.nextRun[key] = now.Add(every)
				m.mu.Unlock()
				continue
			}
			if now.Before(next) {
				m.mu.Unlock()
				continue
			}
			m.nextRun[key] = now.Add(every)
			m.mu.Unlock()
			_, _ = m.Call(ctx, r.ID, Request{Method: "POST", Path: job.Call})
		}
		_ = m.DeliverOutbox(ctx, r.ID)
		// Capability refreshes ride the schedule: list sources and
		// channels declare their own refresh in contributes.
		m.maybeRefresh(ctx, r, now)
	}
}

func (m *Manager) factoryFor(r Record) (RuntimeFactory, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.fatories[r.Manifest.RuntimeKind()]
	return f, ok
}

// mayRunJobs applies cluster placement: leader plugins only on the
// leader, every_node everywhere.
func (m *Manager) mayRunJobs(r Record) bool {
	if r.Manifest.Ether.Cluster == "every_node" {
		return true
	}
	return m.isLeader()
}

func (m *Manager) maybeRefresh(ctx context.Context, r Record, now time.Time) {
	type refresh struct {
		key   string
		every time.Duration
		path  string
	}
	var jobs []refresh
	c := r.Manifest.Ether.Contributes
	if c.Channels != nil && c.Channels.Refresh != "" {
		if d, err := time.ParseDuration(c.Channels.Refresh); err == nil {
			jobs = append(jobs, refresh{"channels", d, "/ether/channels.json"})
		}
	}
	for _, ls := range c.ListSources {
		refreshStr := ls.Refresh
		if refreshStr == "" {
			refreshStr = "1h"
		}
		if d, err := time.ParseDuration(refreshStr); err == nil {
			jobs = append(jobs, refresh{"list:" + ls.Key, d, "/catalog/movie/" + ls.Catalog + ".json"})
		}
	}
	for _, j := range jobs {
		key := r.ID + "|refresh:" + j.key
		m.mu.Lock()
		next, seen := m.nextRun[key]
		if !seen {
			m.nextRun[key] = now.Add(j.every)
			m.mu.Unlock()
			continue
		}
		if now.Before(next) {
			m.mu.Unlock()
			continue
		}
		m.nextRun[key] = now.Add(j.every)
		m.mu.Unlock()
		_, _ = m.Call(ctx, r.ID, Request{Method: "GET", Path: j.path})
	}
}
