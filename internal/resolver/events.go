package resolver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
)

func (s *Service) publishEvent(eventType string, payload any, opts ...events.EnvelopeOption) {
	if s == nil || s.events == nil {
		return
	}
	_ = s.events.PublishTyped(eventType, payload, opts...)
}

func resolverMediaType(t *core.Title) string {
	if t == nil {
		return ""
	}
	switch t.Type {
	case core.TypeMovie:
		return "movie"
	case core.TypeEpisode:
		return "episode"
	default:
		return ""
	}
}

func clientDisplayName(c core.ClientCaps) string {
	switch {
	case c.Device != "":
		return c.Device
	case c.Product != "":
		return c.Product
	case c.ClientID != "":
		return c.ClientID
	case c.Protocol != "":
		return string(c.Protocol)
	default:
		return ""
	}
}

func releaseHDRString(r core.Release) string {
	switch {
	case r.Info.DolbyVision:
		return "dv"
	case r.Info.HDR10:
		return "hdr10"
	default:
		return "sdr"
	}
}

func releaseResolutionString(r core.Release) string {
	if r.Info.Height > 0 {
		return fmt.Sprintf("%dp", r.Info.Height)
	}
	return ""
}

func releaseProtocolString(r core.Release) string {
	if r.Protocol == "" {
		return "usenet"
	}
	return string(r.Protocol)
}

func (s *Service) emitResolveOutcome(req core.ResolveRequest, profile core.QualityProfile, trace Trace, resolved *core.Resolution, resolveErr error) {
	if s == nil || s.events == nil || req.Title == nil {
		return
	}
	titleKey := req.Title.Key.String()
	titleName := trace.TitleName
	if titleName == "" {
		titleName = req.Title.Title
	}
	mediaType := resolverMediaType(req.Title)
	corr := events.WithCorrelation(events.Correlation{TitleID: titleKey})

	eligible := 0
	rejected := 0
	rejectionCounts := map[string]int{}
	topRejection := ""
	topCount := 0
	var winnerTrace *CandidateTrace
	for i := range trace.Candidates {
		c := &trace.Candidates[i]
		if c.Rejected {
			rejected++
			if c.Reason != "" {
				rejectionCounts[c.Reason]++
				if rejectionCounts[c.Reason] > topCount {
					topCount = rejectionCounts[c.Reason]
					topRejection = c.Reason
				}
			}
		} else {
			eligible++
		}
		if resolved != nil && c.ReleaseID == resolved.Release.ID {
			winnerTrace = c
		}
	}

	if resolved != nil {
		cacheHit := trace.SearchDuration == 0
		s.publishEvent(events.EventResolverSearchCompleted, events.ResolverSearchCompletedPayload{
			TitleKey:        titleKey,
			TitleName:       titleName,
			MediaType:       mediaType,
			CandidateCount:  len(trace.Candidates),
			EligibleCount:   eligible,
			WinnerReleaseID: resolved.Release.ID,
			DurationMS:      trace.TotalDuration.Milliseconds(),
			CacheHit:        cacheHit,
		}, corr)

		score := 0
		pinned := trace.Pinned != "" && trace.Pinned == resolved.Release.ID
		if winnerTrace != nil {
			score = int(winnerTrace.Score)
			if winnerTrace.Pinned {
				pinned = true
			}
		}
		s.publishEvent(events.EventResolverReleaseSelected, events.ResolverReleaseSelectedPayload{
			TitleKey:    titleKey,
			ReleaseID:   resolved.Release.ID,
			ReleaseName: resolved.Release.Name,
			Indexer:     resolved.Release.Indexer,
			Protocol:    releaseProtocolString(resolved.Release),
			SizeBytes:   resolved.Release.Size,
			Resolution:  releaseResolutionString(resolved.Release),
			HDR:         releaseHDRString(resolved.Release),
			Score:       score,
			Pinned:      pinned,
		}, corr)
		return
	}

	if resolveErr == nil || errors.Is(resolveErr, context.Canceled) {
		return
	}
	if topRejection == "" && trace.Err != "" {
		topRejection = trace.Err
	}
	if errors.Is(resolveErr, core.ErrNotAvailable) || len(trace.Candidates) == 0 || eligible == 0 {
		indexers := map[string]struct{}{}
		for _, c := range trace.Candidates {
			if c.Indexer != "" {
				indexers[c.Indexer] = struct{}{}
			}
		}
		indexersQueried := len(indexers)
		if indexersQueried == 0 && len(trace.Discovery) > 0 {
			indexersQueried = len(trace.Discovery)
		}
		s.publishEvent(events.EventResolverSearchEmpty, events.ResolverSearchEmptyPayload{
			TitleKey:        titleKey,
			TitleName:       titleName,
			MediaType:       mediaType,
			ProfileID:       profile.Name,
			IndexersQueried: indexersQueried,
			RejectedCount:   rejected,
			TopRejection:    strings.TrimSpace(topRejection),
		}, corr)
	}
}

func (s *Service) emitReleaseBlocklisted(e BlockEntry, actor string) {
	if s == nil || s.events == nil {
		return
	}
	titleKey := ""
	if e.TitleKey != 0 {
		titleKey = e.TitleKey.String()
	}
	s.publishEvent(events.EventResolverReleaseBlocklisted, events.ResolverReleaseBlocklistedPayload{
		ReleaseID:   e.ReleaseID,
		ReleaseName: e.Name,
		TitleKey:    titleKey,
		Indexer:     e.Indexer,
		Reason:      e.Reason,
		Actor:       actor,
	}, events.WithCorrelation(events.Correlation{TitleID: titleKey}))
}

func (s *Service) emitPinCreated(p Pin, actor string) {
	if s == nil || s.events == nil {
		return
	}
	titleKey := p.TitleKey.String()
	s.publishEvent(events.EventResolverPinCreated, events.ResolverPinCreatedPayload{
		TitleKey:    titleKey,
		TitleName:   p.TitleName,
		ReleaseID:   p.Release.ID,
		ReleaseName: p.Release.Name,
		Note:        p.Note,
		Actor:       actor,
	}, events.WithCorrelation(events.Correlation{TitleID: titleKey}))
}

func (s *Service) emitPinRemoved(key core.RatingKey, p Pin, had bool, actor string) {
	if s == nil || s.events == nil {
		return
	}
	titleKey := key.String()
	titleName, releaseID := "", ""
	if had {
		titleName = p.TitleName
		releaseID = p.Release.ID
	}
	s.publishEvent(events.EventResolverPinRemoved, events.ResolverPinRemovedPayload{
		TitleKey:  titleKey,
		TitleName: titleName,
		ReleaseID: releaseID,
		Actor:     actor,
	}, events.WithCorrelation(events.Correlation{TitleID: titleKey}))
}

func (s *Service) emitPinStatusEvent(p Pin, status PinStatus, reason, client string) {
	if s == nil || s.events == nil {
		return
	}
	titleKey := p.TitleKey.String()
	corr := events.WithCorrelation(events.Correlation{TitleID: titleKey})
	switch {
	case status == PinUnhealthy && !p.Fallback:
		// A preference that falls back is kept to be retried, so it must not
		// drive "remove dead pins" flows or critical alerts.
		s.publishEvent(events.EventResolverPinUnhealthy, events.ResolverPinUnhealthyPayload{
			TitleKey:  titleKey,
			TitleName: p.TitleName,
			ReleaseID: p.Release.ID,
			Status:    string(status),
			Reason:    reason,
		}, corr)
	case status == PinRejected:
		s.publishEvent(events.EventResolverPinRejected, events.ResolverPinRejectedPayload{
			TitleKey:  titleKey,
			TitleName: p.TitleName,
			ReleaseID: p.Release.ID,
			Client:    client,
			Reason:    reason,
		}, corr)
	}
}
