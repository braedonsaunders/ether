package livetv

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
)

// breakLoop is the only caller of BreakInput/Analyze. One sequential worker per
// service/node, with a two-hour lookahead and at most ten minutes per decode.
// It updates saved programming with compare-and-swap so a concurrent owner save
// always wins. Requests only consume the resulting immutable item metadata.
func (s *Service) breakLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	retry := map[core.RatingKey]time.Time{}
	var ownerRevision uint64
	for {
		s.mu.Lock()
		revision := s.breakOwnerRevision
		s.mu.Unlock()
		if revision != ownerRevision {
			// An admitted save can unblock setup-only source identity after
			// startup. Do not retain the earlier unauthenticated-job backoff.
			retry = map[core.RatingKey]time.Time{}
			ownerRevision = revision
		}
		for key, at := range retry {
			if !s.o.Now().Before(at) {
				delete(retry, key)
			}
		}
		s.analyzeUpcomingBreaks(retry)
		select {
		case <-s.ctx.Done():
			return
		case observation := <-s.breakObserved:
			s.storeBreakObservation(observation)
		case <-tick.C:
		}
	}
}

func (s *Service) analyzeUpcomingBreaks(retry map[core.RatingKey]time.Time) {
	cs, err := s.virtualChannels(s.ctx)
	if err != nil {
		return
	}
	now := s.o.Now()
	type breakTask struct {
		programme Programme
		ownerID   int64
	}
	var pending []breakTask
	for _, c := range cs {
		if !c.Enabled {
			continue
		}
		eligible := s.channelNaturalBreakKeys(c)
		if len(eligible) == 0 {
			continue
		}
		s.mu.Lock()
		ownerID := s.breakOwners[c.ID]
		s.mu.Unlock()
		for _, p := range Schedule(c, now, now.Add(2*time.Hour)) {
			if p.Item.Key == 0 || !eligible[p.Item.Key] || (p.Item.Key.Kind() != core.KindEpisode && p.Item.ShowKey == 0 && p.Item.MediaType != "episode") || !p.Start.After(now.Add(time.Minute)) {
				continue
			}
			if observed, ok := s.observedBreaks[p.Item.Key]; ok {
				// A later airing can reuse observed points without reopening source bytes.
				s.publishBreaks(p.Item.Key, observed.points, observed.input.Info.Duration.Milliseconds(), observed.input.ReleaseID, observed.input.Revision)
				// Retain bounded observations until all channels can publish safely.
			}
			if now.Before(retry[p.Item.Key]) {
				continue
			}
			pending = append(pending, breakTask{programme: p, ownerID: ownerID})
		}
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].programme.Start.Before(pending[j].programme.Start) })
	done := map[core.RatingKey]bool{}
	admitted := 0
	for _, task := range pending {
		p := task.programme
		if s.ctx.Err() != nil {
			return
		}
		if done[p.Item.Key] || !p.Start.After(s.o.Now().Add(time.Minute)) {
			continue
		}
		done[p.Item.Key] = true
		if admitted >= 64 {
			break
		}
		admitted++
		retry[p.Item.Key] = s.o.Now().Add(15 * time.Minute)
		ctx, cancel := context.WithTimeout(s.ctx, min(10*time.Minute, p.Start.Sub(s.o.Now())-time.Minute))
		in, e := s.o.BreakInput(ctx, p.Item.Key, task.ownerID)
		if e != nil {
			cancel()
			continue
		}
		points, e := s.o.BreakAnalyzer.Analyze(ctx, in)
		if in.Close != nil {
			in.Close()
		}
		cancel()
		if e != nil {
			s.o.Logger.Debug("act-break analysis unavailable", "key", p.Item.Key)
			continue
		}
		retry[p.Item.Key] = s.o.Now().Add(6 * time.Hour)
		s.publishBreaks(p.Item.Key, points, in.Info.Duration.Milliseconds(), in.ReleaseID, in.Revision)
	}
}

// Only InputUser can resolve the setup principal: it requires the admin guard's
// request admission. Retain just its identity, never the request or its values.
func (s *Service) captureBreakOwner(ctx context.Context, channelID string) {
	if s.o.InputUser == nil {
		return
	}
	u, err := s.o.InputUser(ctx)
	if err != nil || u == nil || !u.IsOwner || u.ID <= 0 {
		return
	}
	s.mu.Lock()
	if s.breakOwners == nil {
		s.breakOwners = make(map[string]int64)
	}
	if s.breakOwners[channelID] != u.ID {
		s.breakOwners[channelID] = u.ID
		s.breakOwnerRevision++
	}
	s.mu.Unlock()
}

func (s *Service) publishBreaks(key core.RatingKey, points []markers.BreakPoint, duration int64, releaseID, revision string) {
	cs, err := s.virtualChannels(s.ctx)
	if err != nil {
		return
	}
	for _, c := range cs {
		var raw string
		if s.o.DB.QueryRowContext(s.ctx, `SELECT data FROM livetv_channels WHERE id=?`, c.ID).Scan(&raw) != nil {
			continue
		}
		if json.Unmarshal([]byte(raw), &c) != nil {
			continue
		}
		// Traverse materialized source snapshots too (including nested mixes,
		// pins and block overrides), without discarding additive JSON fields.
		var document any
		if json.Unmarshal([]byte(raw), &document) != nil {
			continue
		}
		changed := false
		activeKnown := false
		// Include the preceding content slice: an in-programme commercial pod
		// has key zero but still belongs to the existing act timeline.
		for _, airing := range Schedule(s.graphicsChannel(c), s.o.Now().Add(-time.Duration(duration)*time.Millisecond-time.Hour), s.o.Now().Add(time.Millisecond)) {
			if airing.Item.Key != 0 {
				activeKnown = airing.Item.Key == key && len(airing.Item.BreakPointsMS) > 0
			}
		}
		walkNaturalBreakItems(document, c.Look, func(v map[string]any, policy *ChannelLook) {
			id, ok := v["key"].(float64)
			if !ok || core.RatingKey(id) != key {
				return
			}
			d, _ := v["durationMs"].(float64)
			if d <= 0 {
				return
			}
			usable := min(duration, int64(d))
			k := max(1, int((usable+1799999)/1800000)) * policy.BreakPattern.PodsPerHalfHour
			selected := markers.SelectBreaks(points, usable, k, max(int64(180000), usable/int64(k+1)/2))
			offsets := make([]int64, 0, len(selected))
			for _, point := range selected {
				offsets = append(offsets, point.AtMS)
			}
			oldJSON, _ := json.Marshal(v["breakPoints"])
			newJSON, _ := json.Marshal(selected)
			if string(oldJSON) == string(newJSON) && v["breakReleaseId"] == releaseID && v["breakRevision"] == revision {
				return
			}
			if activeKnown {
				return
			}
			v["breakAvailableAfterMs"] = s.o.Now().Add(time.Minute).UnixMilli()
			v["breakPoints"] = selected
			v["breakPointsMs"] = offsets
			v["breakReleaseId"] = releaseID
			v["breakRevision"] = revision
			changed = true
		})
		if !changed {
			continue
		}
		b, e := json.Marshal(document)
		if e != nil || string(b) == raw {
			continue
		}
		result, e := s.o.DB.ExecContext(s.ctx, `UPDATE livetv_channels SET data=? WHERE id=? AND CAST(data AS TEXT)=?`, string(b), c.ID, raw)
		if e == nil {
			if n, _ := result.RowsAffected(); n > 0 {
				s.invalidate(c.ID, "")
				s.bumpLineup()
			}
		}
	}
}

func naturalBreakPolicy(p *ChannelLook) bool {
	return p != nil && p.BreakPattern != nil && p.BreakPattern.Inside && (p.BreakPlacement == "" || p.BreakPlacement == "natural")
}

// A non-null block/pin breaks object replaces the entire break policy.
func walkNaturalBreakItems(node any, policy *ChannelLook, visit func(map[string]any, *ChannelLook)) {
	switch v := node.(type) {
	case []any:
		for _, child := range v {
			walkNaturalBreakItems(child, policy, visit)
		}
	case map[string]any:
		if override, ok := v["breaks"].(map[string]any); ok {
			b, _ := json.Marshal(override)
			var p ChannelLook
			if json.Unmarshal(b, &p) == nil {
				policy = &p
			}
		}
		if _, ok := v["durationMs"]; ok && naturalBreakPolicy(policy) {
			visit(v, policy)
			return
		}
		for _, child := range v {
			walkNaturalBreakItems(child, policy, visit)
		}
	}
}
func (s *Service) channelNaturalBreakKeys(c Channel) map[core.RatingKey]bool {
	keys := map[core.RatingKey]bool{}
	var raw string
	if s.o.DB.QueryRowContext(s.ctx, `SELECT data FROM livetv_channels WHERE id=?`, c.ID).Scan(&raw) != nil {
		return keys
	}
	var doc any
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return keys
	}
	walkNaturalBreakItems(doc, c.Look, func(item map[string]any, _ *ChannelLook) {
		if key, ok := item["key"].(float64); ok {
			keys[core.RatingKey(key)] = true
		}
	})
	return keys
}
