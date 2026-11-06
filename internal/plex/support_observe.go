package plex

import (
	"context"

	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/store"
)

func (s *Server) observeRequest(r *http.Request, sr *statusRecorder, elapsed time.Duration) {
	if s == nil || s.d.Support == nil || r == nil || sr == nil {
		return
	}
	pr := FromContext(r.Context())
	status := sr.status
	if status == 0 {
		status = http.StatusOK
		if r.Context().Err() != nil {
			// Observation only: the disconnected client received no response.
			status = 499
		}
	}
	kind := diagnostics.ClassifyRequest(r.URL.Path, status, sr.Header().Get("Content-Type"), sr.written)
	recording := s.d.Support.Recording()
	ev := s.supportEvent(pr, kind)
	ev.Method = r.Method
	ev.Route = r.URL.Path
	ev.Status = status
	ev.Bytes = sr.written
	if recording {
		if kind != diagnostics.KindDevice {
			s.d.Support.Observe(ev)
		}
		ev.Kind = diagnostics.KindRequest
		ev.Duration = elapsed
		ev.Cancelled = r.Context().Err() != nil
		ev.Query = diagnostics.SafeQuery(r.URL.Query())
		if stats := store.QueryStatsFrom(r.Context()); stats != nil {
			ev.DBQueries = stats.Queries()
			ev.DBTime = stats.DBTime()
		}
		if sr.sampling {
			ev.Response = summarizePlexResponse(sr.sample, sr.truncated, sr.Header().Get("Content-Encoding"))
		}
	}
	s.d.Support.Observe(ev)
}

func (s *Server) observePlaybackReported(pr *Request) {
	if s == nil || s.d.Support == nil || pr == nil || pr.User == nil {
		return
	}
	s.d.Support.Observe(s.supportEvent(pr, diagnostics.KindPlaybackReported))
}

// observePlaybackDecision records a structured resolution for the support
// capture. It does not copy the username or peer onto the event: the
// playback object is exported even when those identity flags are off.
func (s *Server) observePlaybackDecision(ctx context.Context, pr *Request, p *playRequest, ps *core.PlaySession) {
	if s == nil || s.d.Support == nil || s.d.DecisionTraces == nil || pr == nil || p == nil {
		return
	}
	tr, ok := s.d.DecisionTraces.LatestDecision(p.key)
	if !ok {
		return
	}
	requested := p.partReleaseID
	if requested == "" && p.mediaIndex > 0 {
		requested = s.resolveVariantSelection(ctx, pr, p)
	}
	pb := diagnostics.PlaybackDecision{
		Profile:            tr.Profile,
		ProfileMaxHeight:   tr.ProfileMaxHeight,
		ClientMaxHeight:    tr.ClientMaxHeight,
		EffectiveMaxHeight: tr.EffectiveMaxHeight,
		AllowHDR:           tr.AllowHDR,
		AllowDolbyVision:   tr.AllowDolbyVision,
		Chooser:            tr.Chooser,
		ChosenRelease:      tr.ChosenVersion,
		RequestedPartID:    p.requestedPart,
		Variants:           s.playbackVariants(pr, p.key, tr, requested, p.requestedPart),
	}
	for _, c := range tr.Candidates {
		pb.Candidates = append(pb.Candidates, diagnostics.PlaybackCandidate{
			Name: c.Name, Height: c.Height, Size: c.Size, Score: c.Score,
			Rejected: c.Rejected, Reason: c.Reason, Healthy: c.Healthy, HealthChecked: c.HealthChecked,
		})
	}
	ev := diagnostics.Event{
		Time: s.now(), Kind: diagnostics.KindPlaybackDecision,
		DeviceKey: pr.ClientID, SessionKey: pr.SessionID,
		Product: pr.Product, Platform: pr.Platform, Model: pr.Model,
		Playback: &pb,
	}
	if ev.SessionKey == "" && ps != nil {
		ev.SessionKey = ps.ID
	}
	s.d.Support.Observe(ev)
}

func (s *Server) playbackVariants(pr *Request, key core.RatingKey, tr core.DecisionTrace, requested string, requestedPart int64) []diagnostics.PlaybackVariant {
	if pr == nil || pr.User == nil || s.playIDs == nil {
		return nil
	}
	ids, ok := s.playIDs.releases(s.playIdentityFor(pr, key), s.now())
	if !ok || len(ids) == 0 {
		return nil
	}
	byID := make(map[string]core.DecisionCandidate, len(tr.Candidates))
	for _, c := range tr.Candidates {
		if c.ReleaseID != "" {
			byID[c.ReleaseID] = c
		}
	}
	out := make([]diagnostics.PlaybackVariant, 0, len(ids))
	for i, id := range ids {
		c := byID[id]
		auto := i == 0
		part := int64(key)
		if !auto && s.variantIDs != nil {
			part = s.variantIDs.idFor(key, id)
		}
		out = append(out, diagnostics.PlaybackVariant{
			PartID: part, Auto: auto, Height: c.Height, Release: c.Name,
			Requested: (requested != "" && id == requested) || (requestedPart != 0 && part == requestedPart),
		})
	}
	return out
}

func (s *Server) supportEvent(pr *Request, kind diagnostics.Kind) diagnostics.Event {
	ev := diagnostics.Event{Time: s.now(), Kind: kind, Protocol: "plex"}
	if pr == nil {
		return ev
	}
	ev.DeviceKey = pr.ClientID
	ev.SessionKey = pr.SessionID
	ev.DeviceName = pr.DeviceName
	ev.Device = pr.Device
	if ev.DeviceName == "" {
		ev.DeviceName = pr.Device
	}
	ev.Product = pr.Product
	ev.Model = pr.Model
	ev.Platform = pr.Platform
	ev.AppVersion = pr.Version
	if pr.RemoteIP.IsValid() {
		ev.Peer = pr.RemoteIP.String()
	}
	if pr.User != nil {
		ev.Username = pr.User.Username
		ev.ViewerID = pr.User.ID
	}
	return ev
}
