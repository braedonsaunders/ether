package state

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// pendingWrites serializes and coalesces watch changes by viewer and item.
// It is used for standalone SQLite, where another writer can hold the gate
// long enough to outlive a player's final request.
const pendingCapacity = 4096

type pendingWrite struct {
	progress progressWrite
	kind     string // progress, scrobble or unscrobble
	seq      uint64
}
type pendingWrites struct {
	mu      sync.Mutex
	entries map[progressKey]pendingWrite
	seq     uint64
	closed  bool
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	s       *State
}

func newPendingWrites(s *State) *pendingWrites {
	p := &pendingWrites{s: s, entries: make(map[progressKey]pendingWrite), wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go p.run()
	return p
}
func (p *pendingWrites) put(k progressKey, w pendingWrite) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("state: pending writes closed")
	}
	old, ok := p.entries[k]
	if !ok && len(p.entries) >= pendingCapacity {
		p.s.log.Warn("state: pending watch writes full", "capacity", pendingCapacity)
		return errors.New("state: pending watch writes full")
	}
	// A stale progress report cannot undo an outstanding terminal report.
	if ok && w.kind == "progress" {
		if old.kind != "progress" {
			return nil
		}
		if len(w.progress.state) > 5 && w.progress.state[:5] == "plex-" && len(old.progress.state) > 5 && old.progress.state[:5] == "plex-" {
			// Preserve committed intentions when a new play starts before the
			// held completion reaches SQLite. Ordinary progress still coalesces.
			w.progress = mergeProgress(old.progress, w.progress)
		} else if old.progress.completes() && !w.progress.completes() {
			return nil
		}
		if old.progress.state == "stopped" && w.progress.state != "stopped" && !w.progress.completes() {
			return nil
		}
	}
	if ok && w.kind != "progress" && old.kind == "progress" {
		before := old.progress
		before.before = nil
		w.progress.before = &before
	}
	p.seq++
	w.seq = p.seq
	p.entries[k] = w
	p.s.gen.Add(1)
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}
func (p *pendingWrites) snapshot(user int64, keys []core.RatingKey) map[core.RatingKey]pendingWrite {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[core.RatingKey]pendingWrite)
	for _, key := range keys {
		if w, ok := p.entries[progressKey{user, key}]; ok {
			out[key] = w
		}
	}
	return out
}
func (p *pendingWrites) userKeys(user int64) []core.RatingKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	var keys []core.RatingKey
	for k := range p.entries {
		if k.user == user {
			keys = append(keys, k.key)
		}
	}
	return keys
}
func (p *pendingWrites) next() (progressKey, pendingWrite, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var key progressKey
	var w pendingWrite
	found := false
	for k, v := range p.entries {
		if !found || v.seq < w.seq {
			key, w, found = k, v, true
		}
	}
	return key, w, found
}
func (p *pendingWrites) apply(ctx context.Context, k progressKey, w pendingWrite) error {
	ctx = WithOrigin(ctx, w.progress.origin)
	if w.progress.before != nil {
		var watchAt int64
		err := p.s.db.QueryRowContext(ctx, `SELECT watch_at FROM state_watch WHERE user_id=? AND rating_key=?`, k.user, int64(k.key)).Scan(&watchAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) || unixTime(watchAt).Before(w.progress.before.at) {
			if err := p.s.applyProgressChain(ctx, k, *w.progress.before); err != nil {
				return err
			}
		}
	}
	switch w.kind {
	case "scrobble":
		return p.s.scrobbleAt(ctx, k.user, k.key, w.progress.at)
	case "unscrobble":
		return p.s.unscrobbleAt(ctx, k.user, k.key, w.progress.at)
	default:
		return p.s.progressAt(ctx, k.user, k.key, w.progress)
	}
}
func (p *pendingWrites) remove(k progressKey, seq uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w, ok := p.entries[k]; ok && w.seq == seq {
		delete(p.entries, k)
	}
}
func (p *pendingWrites) run() {
	defer close(p.done)
	for {
		k, w, ok := p.next()
		if !ok {
			select {
			case <-p.stop:
				return
			case <-p.wake:
				continue
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		err := p.apply(ctx, k, w)
		cancel()
		if err == nil {
			p.remove(k, w.seq)
			continue
		}
		if !store.IsWriteContention(err) {
			// Only a busy writer is worth waiting for; any other failure would
			// hold every later watch write behind this one forever.
			p.s.log.Warn("state: dropping watch write that cannot be applied", "user", k.user, "key", k.key, "kind", w.kind, "err", err)
			p.remove(k, w.seq)
			continue
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-p.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (p *pendingWrites) close(ctx context.Context) error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.stop)
	}
	p.mu.Unlock()
	select {
	case <-p.done:
	case <-ctx.Done():
		p.s.log.Warn("state: pending watch writes could not be flushed", "err", ctx.Err())
		return ctx.Err()
	}
	for {
		k, w, ok := p.next()
		if !ok {
			return nil
		}
		if err := p.apply(ctx, k, w); err != nil {
			if store.IsWriteContention(err) {
				p.s.log.Warn("state: pending watch writes could not be flushed", "err", err)
				return err
			}
			p.s.log.Warn("state: dropping watch write that cannot be applied", "user", k.user, "key", k.key, "kind", w.kind, "err", err)
		}
		p.remove(k, w.seq)
	}
}
func overlayPending(ws core.WatchStatus, w pendingWrite, counted bool, watchAt time.Time) core.WatchStatus {
	// A read racing a just-committed entry must not apply it twice.
	if !watchAt.Before(w.progress.at) {
		return ws
	}
	if w.progress.before != nil {
		ws = overlayPending(ws, pendingWrite{kind: "progress", progress: *w.progress.before}, counted, watchAt)
	}
	switch w.kind {
	case "scrobble":
		ws.ViewCount++
		ws.ViewOffset = 0
	case "unscrobble":
		ws.ViewCount = 0
		ws.ViewOffset = 0
	default:
		if len(w.progress.state) > 5 && w.progress.state[:5] == "plex-" {
			if w.progress.duration > 0 {
				ws.Duration = w.progress.duration
			}
			switch w.progress.state {
			case "plex-resume":
				ws.ViewOffset = w.progress.offset
				ws.LastViewedAt = w.progress.at
			case "plex-complete":
				ws.ViewOffset = 0
				ws.ViewCount++
				ws.LastViewedAt = w.progress.at
			case "plex-clear":
				ws.ViewOffset = 0
			case "plex-offline-clear":
				ws.ViewOffset = 0
				ws.ViewCount = 0
			}
			ws.UpdatedAt = w.progress.at
			return ws
		}
		if w.progress.state == "progress" {
			if w.progress.offset <= time.Minute {
				return ws
			}
			ws.ViewOffset = w.progress.offset
			break
		}
		if w.progress.duration > 0 {
			ws.Duration = w.progress.duration
		}
		offset := max(w.progress.offset, 0)
		if ws.Duration > 0 && offset >= time.Duration(WatchedThreshold*float64(ws.Duration)) {
			if !counted {
				ws.ViewCount++
			}
			ws.ViewOffset = 0
		} else if ws.Duration > 0 && offset < time.Duration(StartedThreshold*float64(ws.Duration)) {
			ws.ViewOffset = 0
		} else {
			ws.ViewOffset = offset
		}
	}
	ws.UpdatedAt = w.progress.at
	if w.kind != "unscrobble" {
		ws.LastViewedAt = w.progress.at
	}
	return ws
}
func sortPendingStatuses(statuses []core.WatchStatus) {
	sort.SliceStable(statuses, func(i, j int) bool {
		if statuses[i].LastViewedAt.Equal(statuses[j].LastViewedAt) {
			return statuses[i].UpdatedAt.After(statuses[j].UpdatedAt)
		}
		return statuses[i].LastViewedAt.After(statuses[j].LastViewedAt)
	})
}
