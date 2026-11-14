package livetv

import (
	"context"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type channelReadiness struct {
	until   time.Time
	next    time.Time
	guide   bool
	viewers map[int64]*core.User
}

// Only demand wakes this worker. Cached verdicts are reused; misses use the
// budgeted resolver check, serialized here and paced before every real check.
func (s *Service) readinessLoop() {
	defer s.wg.Done()
	rows, err := s.o.DB.QueryContext(s.serviceContext(), `SELECT channel_id,active_until FROM livetv_readiness WHERE active_until>?`, s.o.Now().UnixMilli())
	if err == nil {
		for rows.Next() {
			var id string
			var ms int64
			if rows.Scan(&id, &ms) == nil {
				s.mu.Lock()
				r := s.readiness[id]
				if loaded := time.UnixMilli(ms); loaded.After(r.until) {
					r.until = loaded
				}
				s.readiness[id] = r
				s.mu.Unlock()
			}
		}
		rows.Close()
	}
	for {
		s.mu.Lock()
		active := len(s.readiness) > 0
		if !active {
			clear(s.readyTitles)
			clear(s.programmeSubs)
		}
		s.mu.Unlock()
		var tick <-chan time.Time
		var timer *time.Timer
		if active {
			timer = time.NewTimer(time.Minute)
			tick = timer.C
		}
		select {
		case <-s.serviceContext().Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.readinessWake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
		s.mu.Lock()
		ids := make([]string, 0, len(s.readiness))
		now := s.o.Now()
		for id, r := range s.readiness {
			if !r.until.After(now) {
				delete(s.readiness, id)
				continue
			}
			if !r.next.After(now) {
				ids = append(ids, id)
				r.next = now.Add(time.Minute)
				s.readiness[id] = r
			}
		}
		s.mu.Unlock()
		for _, id := range ids {
			if !s.readinessWatched(id) {
				continue
			}
			c, err := s.Channel(s.serviceContext(), id)
			if err != nil || c.Kind != "virtual" {
				continue
			}
			if c.Personal == nil {
				s.virtualGuide.mu.RLock()
				missing := s.virtualGuide.entries[c.ID] == nil
				s.virtualGuide.mu.RUnlock()
				if missing {
					if _, err := s.withVirtualGuide(s.serviceContext(), []Channel{c}); err != nil {
						continue
					}
				}
			}
			c = s.plannedChannel(c)
			s.mu.Lock()
			r := s.readiness[id]
			s.mu.Unlock()
			_, _ = s.o.DB.ExecContext(s.serviceContext(), `INSERT INTO livetv_readiness(channel_id,active_until) VALUES(?,?) ON CONFLICT(channel_id) DO UPDATE SET active_until=excluded.active_until`, id, r.until.UnixMilli())
			ps := s.readinessSchedule(c, now, now.Add(3*time.Hour))
			if c.Personal != nil {
				s.mu.Lock()
				viewers := make([]*core.User, 0, len(r.viewers))
				for _, u := range r.viewers {
					viewers = append(viewers, u)
				}
				s.mu.Unlock()
				if len(viewers) == 0 {
					rows, err := s.o.DB.QueryContext(s.serviceContext(), `SELECT DISTINCT viewer_id FROM livetv_personal WHERE channel_id=? LIMIT 64`, c.ID)
					if err == nil {
						for rows.Next() {
							var uid int64
							if rows.Scan(&uid) == nil {
								viewers = append(viewers, &core.User{ID: uid})
							}
						}
						rows.Close()
					}
				}
				for _, u := range viewers {
					personal, _ := s.personalProgrammesLimit(s.serviceContext(), c, u, now, now.Add(3*time.Hour), 0)
					ps = append(ps, s.readyProgrammes(c, personal)...)
				}
			}
			s.mu.Lock()
			later := s.readiness[id].guide
			rr := s.readiness[id]
			rr.guide = false
			s.readiness[id] = rr
			s.mu.Unlock()
			if later {
				seen := map[string]bool{}
				for _, p := range s.readinessSchedule(c, now.Add(3*time.Hour), now.Add(8*24*time.Hour)) {
					if !seen[p.BlockID] {
						seen[p.BlockID] = true
						ps = append(ps, p)
					}
				}
			}
			for _, p := range ps {
				if p.Item.Key != 0 {
					if !s.readinessWatched(id) {
						break
					}
					s.checkReady(s.serviceContext(), p.Item.Key)
				}
				select {
				case <-s.serviceContext().Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
			for i, it := range c.Fallback {
				if i >= 12 {
					break
				}
				if !s.readinessWatched(id) {
					break
				}
				s.checkReady(s.serviceContext(), it.Key)
			}
			s.replaceUnready(c, ps)
		}
	}
}

// Guide browsing may compile schedules, but must not acquire unrelated media.
// Recheck between every paced lookup so closing a viewer ends provider work.
func (s *Service) readinessWatched(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ss := range s.sessions {
		if ss.info.Channel.ID == id {
			return true
		}
	}
	return false
}

func (s *Service) demandReadinessLocked(id string, duration time.Duration) {
	if s.readiness == nil {
		s.readiness = map[string]channelReadiness{}
	}
	r := s.readiness[id]
	if duration == 24*time.Hour {
		r.guide = true
	}
	until := s.o.Now().Add(duration)
	if until.After(r.until) {
		r.until = until
		s.readiness[id] = r
	}
	s.readiness[id] = r
	select {
	case s.readinessWake <- struct{}{}:
	default:
	}
}
func (s *Service) checkReady(ctx context.Context, key core.RatingKey) {
	if s.o.Availability == nil {
		return
	}
	known, ready := s.o.Availability.Available(ctx, key)
	if !known && s.o.CheckAvailability != nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		check, cancel := context.WithTimeout(ctx, 45*time.Second)
		known, ready = s.o.CheckAvailability(check, key)
		cancel()
	}
	s.mu.Lock()
	if len(s.readyTitles) > 8192 {
		clear(s.readyTitles)
	}
	if s.readyTitles == nil {
		s.readyTitles = map[core.RatingKey]bool{}
	}
	if known {
		s.readyTitles[key] = ready
	} else {
		delete(s.readyTitles, key)
	}
	s.mu.Unlock()
}
func (s *Service) readinessSchedule(c Channel, from, until time.Time) []Programme {
	s.virtualGuide.mu.RLock()
	ent := s.virtualGuide.entries[c.ID]
	s.virtualGuide.mu.RUnlock()
	if ent == nil {
		return nil
	}
	return s.readyProgrammes(c, ent.playback.slice(c.ID, from, until))
}
func (s *Service) replaceUnready(c Channel, ps []Programme) {
	for _, p := range ps {
		if p.Item.Key == 0 {
			continue
		}
		s.mu.Lock()
		ready, known := s.readyTitles[p.Item.Key]
		s.mu.Unlock()
		if !known || ready {
			continue
		}
		pool := c.Items
		if c.Personal != nil {
			pool = nil
			for _, candidate := range ps {
				if candidate.Rule == p.Rule && candidate.readinessViewer == p.readinessViewer {
					pool = append(pool, candidate.Item)
				}
			}
		}
		for _, b := range c.Blocks {
			if b.ID == p.BlockID {
				pool = b.Items
				break
			}
		}
		start := 0
		for i, it := range pool {
			if it.Key == p.Item.Key {
				start = i + 1
				break
			}
		}
		for n := 0; n < len(pool) && n < 64; n++ {
			it := pool[(start+n)%len(pool)]
			s.mu.Lock()
			ok, known := s.readyTitles[it.Key]
			s.mu.Unlock()
			// Guides can use readiness already learned by playback. Only an
			// active viewer may cause a new provider lookup.
			if (!known || !ok) && s.readinessWatched(c.ID) {
				select {
				case <-s.serviceContext().Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
				if !s.readinessWatched(c.ID) {
					return
				}
				s.checkReady(s.serviceContext(), it.Key)
				s.mu.Lock()
				ok = s.readyTitles[it.Key]
				s.mu.Unlock()
			}
			if !ok || it.Key == p.Item.Key {
				continue
			}
			sub, ok := joinStandIn(p, it, s.o.Now())
			if !ok {
				continue
			}
			s.latchProgramme(sub)
			break
		}
	}
}
func (s *Service) latchProgramme(p Programme) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.programmeSubs == nil {
		s.programmeSubs = map[string]Programme{}
	}
	s.programmeSubs[programmeReadinessKey(p)] = p
	s.lineupUnix = s.o.Now().Unix()
	for k, v := range s.programmeSubs {
		if v.End.Before(s.o.Now()) {
			delete(s.programmeSubs, k)
		}
	}
}
func (s *Service) readyProgrammes(c Channel, ps []Programme) []Programme {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range ps {
		if sub, ok := s.programmeSubs[programmeReadinessKey(p)]; ok {
			ps[i] = sub
		}
	}
	return ps
}

// Preserve the original airing interval. Shorter media joins as far in as
// possible while reserving two minutes; a mid-slot replacement never restarts.
func joinStandIn(slot Programme, item Item, now time.Time) (Programme, bool) {
	elapsed := now.Sub(slot.Start)
	if elapsed < 0 {
		elapsed = 0
	}
	runtime := time.Duration(item.DurationMS) * time.Millisecond
	offset := elapsed
	if elapsed >= 30*time.Second {
		if runtime <= 2*time.Minute {
			return Programme{}, false
		}
		if offset > runtime-2*time.Minute {
			offset = runtime - 2*time.Minute
		}
		if offset <= 0 {
			return Programme{}, false
		}
	}
	p := slot
	p.Item = item
	p.policyTag = nil
	p.sourceOffset = offset - elapsed
	return p, true
}

func (s *Service) demandPersonalReadiness(c Channel, u *core.User) {
	if u == nil || u.ID <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demandReadinessLocked(c.ID, 24*time.Hour)
	r := s.readiness[c.ID]
	if r.viewers == nil {
		r.viewers = map[int64]*core.User{}
	}
	r.viewers[u.ID] = &core.User{ID: u.ID}
	s.readiness[c.ID] = r
}

func programmeReadinessKey(p Programme) string {
	return p.ChannelID + "/" + strconv.FormatInt(p.readinessViewer, 10) + "/" + p.ID
}
