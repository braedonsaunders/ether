package resolver

import (
	"context"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const channelReadyTTL = 5 * time.Minute

// ReadyChannelRelease reads existing discovery and verification evidence only.
// It never searches, probes a source, or queues an uncached download.
func (s *Service) ReadyChannelRelease(ctx context.Context, req core.ResolveRequest) (core.Release, bool) {
	if req.Title == nil || req.Client.BroadcastBitrateKbps <= 0 {
		return core.Release{}, false
	}
	sreq := searchRequestFor(req.Title)
	results, _ := s.peekSearchCache(searchCacheKey(sreq))
	if req.Prefer != "" {
		if r, ok := s.LookupRelease(req.Prefer); ok {
			results = withRelease(results, r)
		}
	}
	return s.readyChannelFrom(ctx, req, results)
}

// ReadyRememberedChannelRelease validates a recently played source supplied by
// playback after a cold restart; it does not turn a stored title into evidence.
func (s *Service) ReadyRememberedChannelRelease(ctx context.Context, req core.ResolveRequest, r core.Release) (core.Release, bool) {
	got, ok := s.readyChannelFrom(ctx, req, []core.Release{r})
	if ok {
		s.rememberReleases(req.Title, []core.Release{got})
	}
	return got, ok
}

func (s *Service) readyChannelFrom(ctx context.Context, req core.ResolveRequest, results []core.Release) (core.Release, bool) {
	if req.Title == nil || req.Client.BroadcastBitrateKbps <= 0 {
		return core.Release{}, false
	}
	sreq := searchRequestFor(req.Title)
	pool := make([]core.Release, 0, 8)
	for _, r := range results {
		if len(pool) >= 32 {
			break
		}
		if req.Prefer != "" && r.ID != req.Prefer || slices.Contains(req.Exclude, r.ID) || r.Size <= 0 || r.Info.FullSeason || s.isBad(r.ID) || s.recentlyFailedHealth(r.ID) || !s.ChannelSourceAllowed(r, req.Client, req.Title) {
			continue
		}
		at, ok := s.verifiedAt(ctx, r.ID)
		if !ok || s.now().Before(at) || s.now().Sub(at) > channelReadyTTL {
			continue
		}
		if r.Protocol == core.ProtocolTorrent && !s.knownCached(r) && len(r.DebridCached) == 0 {
			continue
		}
		pool = append(pool, r)
	}
	if len(pool) == 0 {
		return core.Release{}, false
	}
	profile := SelectProfileForTitle(s.qualityCfg(), req.Client, req.User, req.Title, s.libProfileForTitle(ctx, req.Title))
	ranked, _, err := s.rankCandidates(ctx, req, sreq, profile, pool, nil, "")
	if err != nil || len(ranked) == 0 {
		return core.Release{}, false
	}
	return ranked[0].rel, true
}

// ResolveReadyChannel opens exactly the locally verified candidate selected by
// the channel race. If its evidence expired, fail rather than starting discovery.
func (s *Service) ResolveReadyChannel(ctx context.Context, req core.ResolveRequest) (*core.Resolution, error) {
	r, ok := s.ReadyChannelRelease(ctx, req)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	payload, err := s.indexer.FetchNZB(core.WithoutOnDemandStreams(ctx), r)
	if err != nil {
		return nil, err
	}
	return &core.Resolution{Title: req.Title.Key, Release: r, NZB: payload, ResolvedAt: s.now()}, nil
}
