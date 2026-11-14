package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Source history is what Ether has learned about each stream source:
// whether it opens, how damaged and how stable its stream is, how much it
// bursts on connect, and how the last session on it went. Direct uses it to
// pick a start mode, Steady to give scarce capacity to sources that need it.
//
// Entries are keyed like the in-memory source health (sourceHealthKey: the
// channel, the source entry and its URL), so a replaced URL starts fresh.
// Reads are lock-free (a sync.Map of immutable entries). Writes update memory
// at once and reach the database write-behind: one transaction per flush
// every historyFlushEvery, never per event, and a synchronous flush on Close.
// Under PostgreSQL (a cluster), each node also merges rows other nodes
// changed, every historyRefreshEvery, reading only rows changed since.

func init() {
	store.Register("livetv", 13, `CREATE TABLE livetv_source_history(key TEXT PRIMARY KEY, updated_ms BIGINT NOT NULL, data TEXT NOT NULL); CREATE INDEX livetv_source_history_updated ON livetv_source_history(updated_ms);`)
}

const (
	historyLimit        = 4096 // entries kept, in memory and in the table
	historyFlushEvery   = 5 * time.Second
	historyRefreshEvery = 30 * time.Second
	historyRetention    = 30 * 24 * time.Hour
	// historyWindow is the media span the rates cover: counters halve when
	// MediaSeconds passes it, so a recovered source stops looking bad.
	historyWindow = 6 * time.Hour
	// historyReportEvery is how often a playing input reports its deltas.
	historyReportEvery = 30 * time.Second
)

// SourceHistory is the learned evidence for one stream source. Counters
// cover roughly the last historyWindow of media received.
type SourceHistory struct {
	Key       string    `json:"key"`
	ChannelID string    `json:"channelId"`
	SourceID  string    `json:"sourceId"`
	Updated   time.Time `json:"updated"`

	// Availability, from every open of the source.
	Failures     int       `json:"failures,omitempty"`     // consecutive failed opens
	OpenFailures int64     `json:"openFailures,omitempty"` // failed opens in the window
	RetryAfter   time.Time `json:"retryAfter,omitempty"`   // cooldown end
	LastError    string    `json:"lastError,omitempty"`    // unavailable, timeout, refused, auth, missing, rate_limited
	LastErrorAt  time.Time `json:"lastErrorAt,omitempty"`
	LastPlayed   time.Time `json:"lastPlayed,omitempty"`

	// Stream evidence, over the media actually received.
	Sessions      int64   `json:"sessions,omitempty"`     // sessions that received media
	MediaSeconds  float64 `json:"mediaSeconds,omitempty"` // the rates' denominator
	DamageEvents  int64   `json:"damageEvents,omitempty"` // see SourceEvidence.DamageEvents
	FormatChanges int64   `json:"formatChanges,omitempty"`
	Stalls        int64   `json:"stalls,omitempty"`
	Reconnects    int64   `json:"reconnects,omitempty"`
	// Keyframe gaps: the longest in the current half-window and the one
	// before it, so a long gap ages out with the other counters.
	GapNow  time.Duration `json:"gapNow,omitempty"`
	GapPrev time.Duration `json:"gapPrev,omitempty"`
	// ConnectBurst is media delivered beyond real time in a connection's
	// first tapBurstProbe (the latest measured): the reserve a raw TS
	// source gives for free.
	ConnectBurst time.Duration `json:"connectBurst,omitempty"`

	// The mode the last session ran in and how it ended (noteSourceMode).
	LastMode    string    `json:"lastMode,omitempty"`
	LastOutcome string    `json:"lastOutcome,omitempty"`
	LastModeAt  time.Time `json:"lastModeAt,omitempty"`
}

// DamagePerHour is damage events per hour of received media.
func (h SourceHistory) DamagePerHour() float64 {
	return perHour(float64(h.DamageEvents), h.MediaSeconds)
}

// StallsPerHour is provider stalls per hour of received media.
func (h SourceHistory) StallsPerHour() float64 { return perHour(float64(h.Stalls), h.MediaSeconds) }

// MaxKeyframeGap is the longest gap between video keyframes in the window.
func (h SourceHistory) MaxKeyframeGap() time.Duration { return max(h.GapNow, h.GapPrev) }

func perHour(n, media float64) float64 {
	if media < 60 {
		// Under a minute of media a rate means little; report the count as
		// if it were an hour's worth rather than inflate it.
		return n
	}
	return n * 3600 / media
}

// SourceEvidence is a batch of observations about one source, added to its
// counters. The tap and relay already record what they see for any input
// read through them (TS continuity and transport errors, sync losses, PMT
// changes, keyframe gaps, stalls, reconnects, received media); callers
// report what only they can see, such as decode-level damage.
type SourceEvidence struct {
	// Media is media received; it is the rates' denominator.
	Media time.Duration
	// DamageEvents counts damage: continuity-counter breaks, transport-error
	// packets, lost sync, failed HLS segments, decode errors.
	DamageEvents int64
	// KeyframeGap is the longest keyframe gap observed (kept as a maximum).
	KeyframeGap   time.Duration
	FormatChanges int64
	Stalls        int64
	Reconnects    int64
	// ConnectBurst, when non-zero, is a fresh connect-burst measurement.
	ConnectBurst time.Duration
	// newSession marks the first report of a session that received media;
	// burstMeasured marks ConnectBurst as a measurement even when zero.
	newSession, burstMeasured bool
}

func (e SourceEvidence) empty() bool { return e == SourceEvidence{} }

type sourceHistoryStore struct {
	s       *Service
	entries sync.Map // key -> *SourceHistory, immutable once stored
	count   atomic.Int64
	mu      sync.Mutex // serialises writers
	dirty   map[string]bool
	flushMu sync.Mutex // one flush at a time
	seen    int64      // newest updated_ms merged from the table (cluster refresh)
	pg      bool
	wake    chan struct{}
}

// sourceHistory reads a source's history without taking any lock.
func (s *Service) sourceHistory(channelID string, src StreamSource) (SourceHistory, bool) {
	v, ok := s.history.entries.Load(sourceHealthKey(channelID, src))
	if !ok {
		return SourceHistory{}, false
	}
	return *v.(*SourceHistory), true
}

// SourceHistories is every entry, newest first (status and the harness).
func (s *Service) SourceHistories() []SourceHistory {
	var out []SourceHistory
	s.history.entries.Range(func(_, v any) bool {
		out = append(out, *v.(*SourceHistory))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// noteSourceEvidence adds observations to a source's counters.
func (s *Service) noteSourceEvidence(channelID string, src StreamSource, ev SourceEvidence) {
	if src.ID == "" || ev.empty() {
		return
	}
	s.history.update(channelID, src, func(h *SourceHistory) { h.add(ev) })
}

// noteSourceMode records the mode a session used on a source and how it
// ended: mode "steady", "copy", "transcode", "passthrough"; outcome "ok",
// "escalated" (switched mid-session), "failed".
func (s *Service) noteSourceMode(channelID string, src StreamSource, mode, outcome string) {
	if src.ID == "" {
		return
	}
	now := s.o.Now()
	s.history.update(channelID, src, func(h *SourceHistory) {
		h.LastMode, h.LastOutcome, h.LastModeAt = mode, outcome, now
	})
}

// noteSourceOpen records one open of the source (recordSourceHealth).
func (s *Service) noteSourceOpen(channelID string, src StreamSource, err error, played bool, retryAfter time.Time) {
	if src.ID == "" {
		return
	}
	now := s.o.Now()
	s.history.update(channelID, src, func(h *SourceHistory) {
		if err != nil {
			h.Failures++
			h.OpenFailures++
			h.LastError, h.LastErrorAt, h.RetryAfter = historyErrorClass(err), now, retryAfter
			return
		}
		h.Failures, h.RetryAfter = 0, time.Time{}
		if played {
			h.LastPlayed = now
		}
	})
}

// historyErrorClass reduces an open failure to a stable class.
func historyErrorClass(err error) string {
	var te *TuneError
	if errors.As(err, &te) {
		switch te.UpstreamStatus {
		case 401, 403:
			return "auth"
		case 404, 410:
			return "missing"
		case 429:
			return "rate_limited"
		case 458, 509:
			return "refused"
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrProviderBusy), errors.Is(err, ErrProviderPaced), errors.Is(err, core.ErrStreamLimit):
		return "refused"
	}
	return "unavailable"
}

func (h *SourceHistory) add(ev SourceEvidence) {
	if ev.newSession {
		h.Sessions++
	}
	h.MediaSeconds += ev.Media.Seconds()
	h.DamageEvents += ev.DamageEvents
	h.FormatChanges += ev.FormatChanges
	h.Stalls += ev.Stalls
	h.Reconnects += ev.Reconnects
	h.GapNow = max(h.GapNow, ev.KeyframeGap)
	if ev.ConnectBurst > 0 || ev.burstMeasured {
		h.ConnectBurst = ev.ConnectBurst
	}
	if h.MediaSeconds >= historyWindow.Seconds() {
		h.MediaSeconds /= 2
		h.DamageEvents /= 2
		h.FormatChanges /= 2
		h.Stalls /= 2
		h.Reconnects /= 2
		h.OpenFailures /= 2
		h.GapPrev, h.GapNow = h.GapNow, 0
	}
}

// update applies fn to a copy of the entry and publishes the copy.
func (h *sourceHistoryStore) update(channelID string, src StreamSource, fn func(*SourceHistory)) {
	key := sourceHealthKey(channelID, src)
	h.mu.Lock()
	var next SourceHistory
	if v, ok := h.entries.Load(key); ok {
		next = *v.(*SourceHistory)
	} else {
		next = SourceHistory{Key: key, ChannelID: channelID, SourceID: src.ID}
		h.count.Add(1)
	}
	fn(&next)
	next.Updated = h.s.o.Now()
	h.entries.Store(key, &next)
	if h.dirty == nil {
		h.dirty = map[string]bool{}
	}
	h.dirty[key] = true
	if h.count.Load() > historyLimit {
		h.evictLocked()
	}
	h.mu.Unlock()
	if h.wake != nil {
		select {
		case h.wake <- struct{}{}:
		default:
		}
	}
}

// evictLocked drops the least recently updated tenth of the entries.
func (h *sourceHistoryStore) evictLocked() {
	var all []*SourceHistory
	h.entries.Range(func(_, v any) bool {
		all = append(all, v.(*SourceHistory))
		return true
	})
	sort.Slice(all, func(i, j int) bool { return all[i].Updated.Before(all[j].Updated) })
	for _, e := range all[:len(all)/10+1] {
		h.entries.Delete(e.Key)
		delete(h.dirty, e.Key)
		h.count.Add(-1)
	}
}

// start loads the table and runs the write-behind (and, on PostgreSQL, the
// cluster refresh) until the service context ends.
func (h *sourceHistoryStore) start(s *Service) {
	db := s.o.DB
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.serviceContext(), 10*time.Second)
	defer cancel()
	cutoff := s.o.Now().Add(-historyRetention).UnixMilli()
	var stale bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM livetv_source_history WHERE updated_ms < ?)`, cutoff).Scan(&stale); err == nil && stale {
		if _, err := db.ExecContext(ctx, `DELETE FROM livetv_source_history WHERE updated_ms < ?`, cutoff); err != nil {
			s.o.Logger.Warn("source history prune failed", "err", err)
		}
	}
	if err := h.load(ctx, db, `SELECT key, updated_ms, data FROM livetv_source_history ORDER BY updated_ms DESC LIMIT ?`, historyLimit); err != nil {
		s.o.Logger.Warn("source history load failed", "err", err)
	}
	h.seedHealth()
	s.wg.Add(1)
	go h.loop(db)
}

// load merges rows into memory, newest wins per key.
func (h *sourceHistoryStore) load(ctx context.Context, db *sql.DB, query string, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, data string
		var updated int64
		if err := rows.Scan(&key, &updated, &data); err != nil {
			return err
		}
		var e SourceHistory
		if json.Unmarshal([]byte(data), &e) != nil || e.Key != key {
			continue
		}
		h.mu.Lock()
		if v, ok := h.entries.Load(key); !ok || !v.(*SourceHistory).Updated.After(e.Updated) {
			if !ok {
				h.count.Add(1)
			}
			h.entries.Store(key, &e)
		}
		if updated > h.seen {
			h.seen = updated
		}
		h.mu.Unlock()
	}
	return rows.Err()
}

// seedHealth restores cooldowns and failure counts into the in-memory source
// health, so a restart does not retry a source that was cooling down.
func (h *sourceHistoryStore) seedHealth() {
	s := h.s
	now := s.o.Now()
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	h.entries.Range(func(_, v any) bool {
		e := v.(*SourceHistory)
		if e.Failures == 0 && e.LastPlayed.IsZero() {
			return true
		}
		if s.sourceHealth == nil {
			s.sourceHealth = map[string]streamSourceHealth{}
		}
		if _, ok := s.sourceHealth[e.Key]; ok || len(s.sourceHealth) >= historyLimit {
			return true
		}
		st := StreamSourceStatus{ID: e.SourceID, Failures: e.Failures, LastPlayed: e.LastPlayed, LastChecked: e.Updated}
		if e.Failures > 0 {
			st.LastError = "source unavailable"
			if e.LastError == "timeout" {
				st.LastError = "source timed out"
			}
			if e.RetryAfter.After(now) {
				st.RetryAfter = e.RetryAfter
			}
		}
		s.sourceHealth[e.Key] = streamSourceHealth{StreamSourceStatus: st}
		return true
	})
}

func (h *sourceHistoryStore) loop(db *sql.DB) {
	defer h.s.wg.Done()
	flush := h.s.newTimer(historyFlushEvery)
	if !flush.Stop() {
		select {
		case <-flush.C:
		default:
		}
	}
	defer flush.Stop()
	armed := false
	var refreshC <-chan time.Time
	if h.pg {
		refresh := h.s.newTicker(historyRefreshEvery)
		defer refresh.Stop()
		refreshC = refresh.C
	}
	for {
		var flushC <-chan time.Time
		if armed {
			flushC = flush.C
		}
		select {
		case <-h.s.serviceContext().Done():
			return
		case <-h.wake:
			if !armed {
				flush.Reset(historyFlushEvery)
				armed = true
			}
		case <-flushC:
			armed = false
			h.flush(h.s.serviceContext(), db)
			h.mu.Lock()
			stillDirty := len(h.dirty) > 0
			h.mu.Unlock()
			if stillDirty {
				flush.Reset(historyFlushEvery)
				armed = true
			}
		case <-refreshC:
			h.refresh(db)
		}
	}
}

// refresh merges rows other cluster nodes changed since the last one seen.
// It reads a minute back to tolerate clock skew between nodes; only rows
// changed in that span are read, never the whole table.
func (h *sourceHistoryStore) refresh(db *sql.DB) {
	h.mu.Lock()
	since := h.seen - time.Minute.Milliseconds()
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(h.s.serviceContext(), 10*time.Second)
	defer cancel()
	if err := h.load(ctx, db, `SELECT key, updated_ms, data FROM livetv_source_history WHERE updated_ms > ? ORDER BY updated_ms LIMIT ?`, since, historyLimit); err != nil && ctx.Err() == nil {
		h.s.o.Logger.Debug("source history refresh failed", "err", err)
	}
}

// flush writes dirty entries in one transaction. Failed writes stay dirty.
func (h *sourceHistoryStore) flush(ctx context.Context, db *sql.DB) {
	h.flushMu.Lock()
	defer h.flushMu.Unlock()
	h.mu.Lock()
	if len(h.dirty) == 0 {
		h.mu.Unlock()
		return
	}
	batch := make([]SourceHistory, 0, len(h.dirty))
	for key := range h.dirty {
		if v, ok := h.entries.Load(key); ok {
			batch = append(batch, *v.(*SourceHistory))
		}
	}
	h.dirty = map[string]bool{}
	h.mu.Unlock()
	err := h.write(ctx, db, batch)
	if err == nil {
		return
	}
	h.s.o.Logger.Warn("source history write failed", "entries", len(batch), "err", err)
	h.mu.Lock()
	for _, e := range batch {
		if _, ok := h.entries.Load(e.Key); ok {
			h.dirty[e.Key] = true
		}
	}
	h.mu.Unlock()
}

func (h *sourceHistoryStore) write(ctx context.Context, db *sql.DB, batch []SourceHistory) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	newest := int64(0)
	for _, e := range batch {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		ms := e.Updated.UnixMilli()
		newest = max(newest, ms)
		if _, err := tx.ExecContext(ctx, `INSERT INTO livetv_source_history(key, updated_ms, data) VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET updated_ms = excluded.updated_ms, data = excluded.data`, e.Key, ms, string(b)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	h.mu.Lock()
	h.seen = max(h.seen, newest)
	h.mu.Unlock()
	return nil
}

// close flushes what is left; Service.Close calls it after its sessions
// have ended and reported.
func (h *sourceHistoryStore) close() {
	if h.s == nil || h.s.o.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.flush(ctx, h.s.o.DB)
}
