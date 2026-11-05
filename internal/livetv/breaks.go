package livetv

import (
	"context"
	"encoding/json"
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
	for {
		for key, at := range retry {
			if !s.o.Now().Before(at) {
				delete(retry, key)
			}
		}
		s.analyzeUpcomingBreaks(retry)
		select {
		case <-s.ctx.Done():
			return
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
	done := map[core.RatingKey]bool{}
	for _, c := range cs {
		if !c.Enabled || !s.channelHasNaturalBreaks(c) {
			continue
		}
		for _, p := range Schedule(c, now, now.Add(2*time.Hour)) {
			if p.Item.Key == 0 || (p.Item.ShowKey == 0 && p.Item.MediaType != "episode") || !p.Start.After(now.Add(time.Minute)) || done[p.Item.Key] || now.Before(retry[p.Item.Key]) {
				continue
			}
			done[p.Item.Key] = true
			retry[p.Item.Key] = now.Add(15 * time.Minute)
			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
			in, e := s.o.BreakInput(ctx, p.Item.Key)
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
			retry[p.Item.Key] = now.Add(6 * time.Hour)
			s.publishBreaks(p.Item.Key, points, in.Info.Duration.Milliseconds(), in.ReleaseID, in.Revision)
		}
	}
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
func (s *Service) channelHasNaturalBreaks(c Channel) bool {
	var raw string
	if s.o.DB.QueryRowContext(s.ctx, `SELECT data FROM livetv_channels WHERE id=?`, c.ID).Scan(&raw) != nil {
		return false
	}
	var doc any
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return false
	}
	found := false
	walkNaturalBreakItems(doc, c.Look, func(map[string]any, *ChannelLook) { found = true })
	return found
}
