package segcache

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Policy controls retention of bytes already fetched. It never downloads media.
// A budget is a soft limit while live playback or explicit pins protect bytes.
type Policy struct {
	Mode        string
	Retention   time.Duration
	ReplayBoost time.Duration
}

type experience struct {
	Title      string    `json:"title"`
	Size       int64     `json:"size"`
	Plays      int       `json:"plays"`
	LastPlayed time.Time `json:"lastPlayed"`
}

// Metrics count disk-cache reads since this process started (not RAM hits).
type Metrics struct {
	Hits         int64 `json:"hits"`
	Misses       int64 `json:"misses"`
	ServedBytes  int64 `json:"servedBytes"`
	EvictedBytes int64 `json:"evictedBytes"`
}

func (c *Cache) Metrics() Metrics  { c.mu.Lock(); defer c.mu.Unlock(); return c.metrics }
func (c *Cache) Directory() string { return c.dir }

func (c *Cache) SetPolicy(p Policy) {
	c.mu.Lock()
	c.policy = p
	c.mu.Unlock()
	c.Sweep()
}

// RecordPlay counts actual playback admissions, never speculative reads. The
// metadata is local, contains no viewer identity, and survives restarts.
func (c *Cache) RecordPlay(id, title string, size int64) {
	if c.shared != nil {
		c.shared.describe(id, title, size, true)
		return
	}
	c.mu.Lock()
	if c.closed || c.policy.Mode == "off" {
		c.mu.Unlock()
		return
	}
	r := c.releaseLocked(id)
	c.mu.Unlock()
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	c.mu.Lock()
	r.experience.Title, r.experience.Size = title, size
	r.experience.Plays = min(r.experience.Plays+1, 1000000)
	r.experience.LastPlayed = c.now()
	b, err := json.Marshal(r.experience)
	c.mu.Unlock()
	if err != nil {
		c.logger.Warn("save cache playback metadata", "err", err)
		return
	}
	c.queueExperience(r.dir, b)
}

// Describe records a source's display metadata without counting a play.
func (c *Cache) Describe(id, title string, size int64) {
	if c.shared != nil {
		c.shared.describe(id, title, size, false)
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	r := c.releaseLocked(id)
	c.mu.Unlock()
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	c.mu.Lock()
	r.experience.Title = title
	r.experience.Size = size
	b, err := json.Marshal(r.experience)
	c.mu.Unlock()
	if err == nil {
		err = os.MkdirAll(r.dir, 0755)
	}
	if err == nil {
		err = writeFileAtomic(filepath.Join(r.dir, "experience.json"), b, false)
	}
	if err != nil {
		c.logger.Warn("save source metadata", "err", err)
	}
}

// Sweep expires idle releases, retaining live streams and owner/resume pins.
// Dropping entries and choosing victims happen under the index lock; unlinking
// uses the same striped locks as budget eviction, so concurrent Puts survive.
func (c *Cache) queueExperience(dir string, body []byte) {
	if c.expCond == nil {
		if err := os.MkdirAll(dir, 0755); err != nil {
			c.logger.Warn("save cache playback metadata", "err", err)
			return
		}
		if err := writeFileAtomic(filepath.Join(dir, "experience.json"), body, false); err != nil {
			c.logger.Warn("save cache playback metadata", "err", err)
		}
		return
	}
	c.expMu.Lock()
	if c.expPending == nil {
		c.expPending = map[string][]byte{}
	}
	c.expPending[dir] = append([]byte(nil), body...)
	if !c.expWriting {
		c.expWriting = true
		go c.drainExperience()
	}
	c.expMu.Unlock()
}

func (c *Cache) drainExperience() {
	for {
		c.expMu.Lock()
		batch := c.expPending
		c.expPending = nil
		if len(batch) == 0 {
			c.expWriting = false
			c.expCond.Broadcast()
			c.expMu.Unlock()
			return
		}
		c.expMu.Unlock()
		for dir, body := range batch {
			if err := os.MkdirAll(dir, 0755); err != nil {
				c.logger.Warn("save cache playback metadata", "err", err)
				continue
			}
			if err := writeFileAtomic(filepath.Join(dir, "experience.json"), body, false); err != nil {
				c.logger.Warn("save cache playback metadata", "err", err)
			}
		}
	}
}

func (c *Cache) flushExperience() {
	if c.expCond == nil {
		return
	}
	c.expMu.Lock()
	for c.expWriting {
		c.expCond.Wait()
	}
	c.expMu.Unlock()
}

func (c *Cache) Sweep() {
	if c.shared != nil {
		c.shared.sweep(true)
		return
	}
	prot, _ := c.protected()
	c.evictMu.Lock() // a floor measurement must not see these bytes half-freed
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.evictMu.Unlock()
		return
	}
	now := c.now()
	var victims []*entry
	for _, r := range c.rels {
		if c.pinnedLocked(r.id) || prot[r.id] {
			continue
		}
		expired := c.policy.Retention > 0 && now.Sub(time.Unix(0, r.lastUsed)) >= c.policy.Retention
		if c.policy.Mode != "off" && !expired {
			continue
		}
		for _, e := range r.segs {
			c.metrics.EvictedBytes += e.size
			c.dropLocked(e)
			victims = append(victims, e)
		}
	}
	c.mu.Unlock()
	if err := c.deleteFiles(victims); err != nil {
		c.logger.Warn("expire cache segments", "err", err)
	}
	c.evictMu.Unlock()
	c.evict()
}

// Run expires idle bytes even when no new streams arrive.
func (c *Cache) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.shared != nil {
				c.shared.heartbeat()
			}
			c.Sweep()
		}
	}
}

// victimLocked picks the least valuable release, then its oldest segment.
// Per-release lists make selection independent of title size: a 100 GiB
// favourite cannot lose simply because its oldest segments fill a sample.
func (c *Cache) victimLocked(protected map[string]bool) *entry {
	if c.policy.Mode != "adaptive" || c.policy.ReplayBoost <= 0 {
		return c.lru.tail
	}
	now := c.now()
	var best *entry
	var bestScore int64
	for _, r := range c.rels {
		if r.tail == nil || c.pinnedLocked(r.id) || protected[r.id] {
			continue
		}
		boost := time.Duration(0)
		if now.Sub(r.experience.LastPlayed) < 30*24*time.Hour {
			boost = time.Duration(min(max(r.experience.Plays-1, 0), 8)) * c.policy.ReplayBoost
		}
		score := r.lastUsed + int64(boost)
		if best == nil || score < bestScore || (score == bestScore && r.id < best.rel.id) {
			best, bestScore = r.tail, score
		}
	}
	return best
}

func (r *release) pushEntry(e *entry) {
	e.relPrev = nil
	e.relNext = r.head
	if r.head != nil {
		r.head.relPrev = e
	} else {
		r.tail = e
	}
	r.head = e
}
func (r *release) removeEntry(e *entry) {
	if e.relPrev != nil {
		e.relPrev.relNext = e.relNext
	} else {
		r.head = e.relNext
	}
	if e.relNext != nil {
		e.relNext.relPrev = e.relPrev
	} else {
		r.tail = e.relPrev
	}
	e.relPrev, e.relNext = nil, nil
}
