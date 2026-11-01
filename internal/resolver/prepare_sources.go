package resolver

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// sourcePreparer is optional: regular Usenet-only indexers need no extra
// work. Debrid indexers can learn cache availability before ranked health
// checks spend their entire budget on unknown torrent descriptors.
type sourcePreparer interface {
	PrepareSources(context.Context, []core.Release) []core.Release
}

func (s *Service) prepareSources(ctx context.Context, cands []candidate, title *core.Title, tr *Trace, scopes ...*candidateChecks) []candidate {
	p, ok := s.indexer.(sourcePreparer)
	if !ok || len(cands) < 2 {
		return cands
	}
	if prepared := cands[0].prepared; prepared != nil {
		select {
		case <-prepared.done:
			if prepared.result.healthy && prepared.result.err == nil {
				// Discovery already produced a playable leading candidate;
				// descriptor preparation must not add another network wait.
				return cands
			}
		default:
		}
	}
	if cands[0].verified && (s.resolverCfg().SourcePreference != PreferDebrid || protocolOf(cands[0].rel) == core.ProtocolTorrent) {
		return cands
	}
	if s.knownCached(cands[0].rel) {
		// Ranking already chose a cached torrent in the accepted quality
		// tier. Validate it now instead of delaying Play to discover the
		// availability of a dozen lower-ranked descriptors. Validation can
		// still reject a stale cache answer and try the normal alternates.
		return cands
	}
	margin := s.resolverCfg().SourceScoreMargin
	if margin <= 0 {
		margin = defaultSourceScoreMargin
	}
	lead := cands[0]
	// Discovery can only promote an eligible peer of the leading tier.
	// Lower tiers remain fallbacks and resolve on demand if reached.
	eligible := func(c candidate) bool {
		return c.bandwidth >= lead.bandwidth && sameQuality(c, lead) && c.rel.Score >= lead.rel.Score-margin
	}
	var releases []core.Release
	var torrents []int // indexes preparation could promote
	for i, c := range cands {
		if c.rel.Protocol == core.ProtocolTorrent && eligible(c) {
			releases = append(releases, c.rel)
			torrents = append(torrents, i)
			if len(releases) == 12 {
				break
			}
		}
	}
	if len(releases) == 0 {
		return cands
	}
	scope := &candidateChecks{}
	scoped := len(scopes) > 0 && scopes[0] != nil
	if scoped {
		scope = scopes[0]
	} else {
		// No owner outlives this call, so speculative checks stay off
		// (as before) and the preparation goroutine is joined here.
		defer scope.close()
	}
	discoveryStart := time.Now()
	// This is the same independent Usenet hedge that ranked validation
	// would run next. Start its grab/health check while torrent hashes
	// and cache status are discovered, and reuse its exact result later.
	// When it fails, the next eligible Usenet candidate starts at once,
	// mirroring the ranked window instead of idling until discovery ends.
	var hedge *candidateCheck
	hedgeIdx := -1
	startHedge := func(from int) {
		hedge, hedgeIdx = nil, -1
		if !scoped || s.resolverCfg().OptimisticStart {
			return
		}
		// Speculative checks share ranked validation's attempt budget.
		// routeSources moves at most two candidates ahead of a checked
		// one, so starting only below maxHealthAttempts-2 keeps every
		// prep-checked candidate inside the budget after reordering.
		for i := from; i < len(cands) && i < maxHealthAttempts-2; i++ {
			if cands[i].rel.Protocol == core.ProtocolUsenet && cands[i].prepared == nil && eligible(cands[i]) {
				cands[i].prepared = scope.start(ctx, s, cands[i].rel)
				hedge, hedgeIdx = cands[i].prepared, i
				return
			}
		}
	}
	startHedge(0)
	pctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	prep := &pendingPrep{cancel: cancel, done: make(chan []core.Release, 1)}
	scope.preps = append(scope.preps, prep)
	go func() { prep.done <- p.PrepareSources(pctx, releases) }()
	apply := func(prepared []core.Release) []candidate {
		prep.consumed = true
		cancel()
		byID := make(map[string]core.Release, len(prepared))
		for _, r := range prepared {
			byID[r.ID] = r
		}
		for i := range cands {
			if r, ok := byID[cands[i].rel.ID]; ok {
				cands[i].rel.InfoHash, cands[i].rel.MagnetURL = r.InfoHash, r.MagnetURL
			}
		}
		s.cachePreparedSources(title, byID)
		return s.routeSources(cands, title, tr)
	}
	// A healthy hedge may proceed without the preparation outcome only
	// when it is equivalent (tier, edition, bandwidth, score margin) to
	// the current lead and to every torrent preparation could promote,
	// and no verified source is in play. Anything else waits in full.
	canProceed := func(ready int) bool {
		if cands[0].verified {
			return false
		}
		if ready != 0 && !equivalentHedge(cands[0], cands[ready], margin) {
			return false
		}
		for _, ti := range torrents {
			if ti != ready && !equivalentHedge(cands[ti], cands[ready], margin) {
				return false
			}
		}
		return true
	}
	// abandon stops waiting for preparation: ranked validation proceeds
	// with the ready source, its grace counted from discovery start.
	// The scope still cancels and joins the preparation goroutine.
	abandon := func() []candidate {
		select {
		case prepared := <-prep.done:
			return apply(prepared)
		default:
		}
		cancel()
		cands[0].graceStart = discoveryStart
		return cands
	}
	var timer *time.Timer
	var grace <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var hedgeDone <-chan struct{}
		if hedge != nil {
			hedgeDone = hedge.done
		}
		select {
		case prepared := <-prep.done:
			return apply(prepared)
		case <-hedgeDone:
			v, ready := hedge.result, hedgeIdx
			hedge = nil
			if ctx.Err() != nil {
				cancel()
				return cands
			}
			if v.healthy && v.err == nil {
				// An explicit Usenet preference leaves preparation nothing
				// that could displace the routed Usenet lead: routing after
				// discovery still puts the first eligible Usenet release
				// first. A lead already proven healthy proceeds at once.
				if ready == 0 && !cands[0].verified && strings.EqualFold(strings.TrimSpace(s.resolverCfg().SourcePreference), PreferUsenet) {
					return abandon()
				}
				// A healthy backup is in hand; spend no more speculative
				// checks. It may proceed once preparation has had the
				// same grace ranked validation gives a preferred source.
				if canProceed(ready) {
					if wait := time.Until(discoveryStart.Add(rankedSourceGrace)); wait <= 0 {
						return abandon()
					} else if timer == nil {
						timer = time.NewTimer(wait)
						grace = timer.C
					}
				}
			} else {
				startHedge(ready + 1)
			}
		case <-grace:
			return abandon()
		case <-ctx.Done():
			cancel()
			return cands
		}
	}
}

// cachePreparedSources persists torrent hashes and magnets discovered during
// PrepareSources back into the in-memory search cache and recent-release index
// so subsequent Variants() calls see the same cached state and rank the same
// release first.
func (s *Service) cachePreparedSources(title *core.Title, byID map[string]core.Release) {
	if len(byID) == 0 {
		return
	}
	if title != nil {
		key := searchCacheKey(searchRequestFor(title))
		s.mu.Lock()
		if e, ok := s.searchCache[key]; ok {
			updated := make([]core.Release, len(e.results))
			copy(updated, e.results)
			changed := false
			for i := range updated {
				if p, ok := byID[updated[i].ID]; ok {
					if updated[i].InfoHash == "" && p.InfoHash != "" {
						updated[i].InfoHash = p.InfoHash
						changed = true
					}
					if updated[i].MagnetURL == "" && p.MagnetURL != "" {
						updated[i].MagnetURL = p.MagnetURL
						changed = true
					}
				}
			}
			if changed {
				e.results = updated
				s.searchCache[key] = e
			}
		}
		s.mu.Unlock()
	}
	c := &s.con
	c.mu.Lock()
	for id, p := range byID {
		if r, ok := c.recent[id]; ok {
			if r.rel.InfoHash == "" && p.InfoHash != "" {
				r.rel.InfoHash = p.InfoHash
			}
			if r.rel.MagnetURL == "" && p.MagnetURL != "" {
				r.rel.MagnetURL = p.MagnetURL
			}
			c.recent[id] = r
		}
	}
	c.mu.Unlock()
}
