package plex

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"math"
)

func (s *Server) timelineBandwidths(pr *Request, key core.RatingKey, ids []string, ps *core.PlaySession) []model.TimelineBandwidths {
	if pr.Product == "Plex for Windows" || pr.Product == "Plex for Windows Phone" || !s.decisionArmed(pr, key) {
		return nil
	}
	var info core.MediaInfo
	var dec core.Decision
	found := false
	for _, id := range ids {
		if e, ok := s.transcodes.get(id); ok && e.userID == pr.User.ID && e.clientID == pr.ClientID && e.key == key && e.playbackID == pr.SessionID {
			info, dec, found = e.info, e.decision, true
			break
		}
	}
	if !found && ps != nil {
		info = ps.Info
		dec.Kind = ps.Decision
		if ps.DeliveryDecision != nil {
			dec = *ps.DeliveryDecision
		}
		found = true
	}
	if !found {
		return nil
	}
	w, h := 0, 0
	if v := info.Video(); v != nil {
		w, h = v.Width, v.Height
		if !dec.VideoCopy && dec.MaxHeight > 0 && h > dec.MaxHeight {
			w = w * dec.MaxHeight / h
			h = dec.MaxHeight
		}
	}

	return []model.TimelineBandwidths{{Bandwidth: []model.TimelineBandwidth{{Time: 0, Bandwidth: plannedBandwidth(info, dec), Resolution: resolutionLabel(w, h)}}}}
}

func (s *Server) timelineTranscodeSessions(pr *Request, key core.RatingKey, ids []string) []model.TranscodeSess {
	for _, id := range ids {
		if e, ok := s.transcodes.get(id); ok && e.started && e.decision.Kind != core.DecisionDirectPlay && e.userID == pr.User.ID && e.clientID == pr.ClientID && e.key == key && e.playbackID == pr.SessionID {
			ts := transcodeSessionModel(e)
			if e.requested.protocol != "" {
				ts.Protocol = string(e.requested.protocol)
			}
			if e.decision.Container != "" {
				ts.Container = e.decision.Container
			}
			return []model.TranscodeSess{ts}
		}
	}
	return nil
}

func plannedBandwidth(info core.MediaInfo, dec core.Decision) int {
	bitrate := info.BitrateKbps
	factor := 2.1
	if dec.Kind == core.DecisionRemux || dec.Kind == core.DecisionTranscode {
		factor = 1.05
		if !dec.VideoCopy && dec.BitrateKbps > 0 {
			bitrate = dec.BitrateKbps
		}
	}
	scaled := math.Max(0, math.Round(float64(bitrate)*factor))
	rate := int(^uint(0) >> 1)
	if scaled < float64(rate) {
		rate = int(scaled)
	}
	return rate
}

func (s *Server) sessionBandwidth(e playEntry, md model.Metadata) int {
	for _, id := range s.transcodes.forClient(e.user.ID, e.client.ClientID, e.key) {
		if entry, ok := s.transcodes.get(id); ok && entry.playbackID == e.client.SessionID {
			return plannedBandwidth(entry.info, entry.decision)
		}
	}
	if s.d.Playback != nil {
		if ps, ok := s.d.Playback.SessionFor(e.user.ID, e.client.ClientID, e.key); ok && ps != nil {
			dec := core.Decision{Kind: ps.Decision}
			if ps.DeliveryDecision != nil {
				dec = *ps.DeliveryDecision
			}
			return plannedBandwidth(ps.Info, dec)
		}
	}
	if e.node != "" && len(md.Media) > 0 && md.Media[0].Bitrate > 0 {
		dec := core.Decision{Kind: core.DecisionDirectPlay}
		if md.TranscodeSession != nil {
			dec.Kind = core.DecisionRemux
		}
		return plannedBandwidth(core.MediaInfo{BitrateKbps: md.Media[0].Bitrate}, dec)
	}
	return 0
}
