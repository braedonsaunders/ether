package resolver

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

type searchPreparationKey struct{}
type speculativeHealthKey struct{}

// Validate a bounded frontier while discovery is still arriving. Hard profile
// rules and explicit picks remain authoritative; a healthy eligible source can
// finish discovery without waiting for an unrelated slow indexer.
// All workers are cancelled and joined by the Resolve that owns them.
type searchPreparation struct {
	mu         sync.Mutex
	ctx        context.Context
	svc        *Service
	req        core.ResolveRequest
	search     core.SearchRequest
	profile    core.QualityProfile
	disabled   bool
	checks     candidateChecks
	byID       map[string]*candidateCheck
	identities map[string]bool
	releases   map[string]core.Release
	all        []core.Release
	seen       map[string]bool
	ranked     []candidate
	first      time.Time
	closed     bool        // set by close; no check starts after it
	partial    atomic.Bool // set before returning an early Play start
}

// earlyCheckLimit caps speculative checks per Play. Four run at once; a
// failed check frees its slot for the next arrived candidate, so a fast
// indexer's answer is worked through without waiting for slow ones.
const earlyCheckLimit = 32

func newSearchPreparation(ctx context.Context, s *Service, req core.ResolveRequest, search core.SearchRequest, profile core.QualityProfile, pin string) *searchPreparation {
	return &searchPreparation{ctx: ctx, svc: s, req: req, search: search, profile: profile,
		disabled: pin != "" || req.Prefer != "" || s.resolverCfg().OptimisticStart,
		byID:     map[string]*candidateCheck{}, identities: map[string]bool{}, releases: map[string]core.Release{}, seen: map[string]bool{}}
}

// explicitReleasePolicy reports a profile with enabled release conditions.
// Progressive start still applies: a primary-tier healthy hit can finish
// Play without waiting out every indexer. A require-fallback stays behind
// criticalPathDeadline so a delayed primary can still arrive.
func explicitReleasePolicy(profile core.QualityProfile) bool {
	for _, r := range profile.ReleaseRules {
		if !r.Disabled && len(r.Conditions) > 0 {
			return true
		}
	}
	return false
}

func (p *searchPreparation) arrived(batch []core.Release) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled || p.ctx.Err() != nil {
		return
	}
	var added []core.Release
	reorder := false
	for _, r := range batch {
		r = withTarget(r, p.search)
		if p.seen[r.ID] {
			if p.svc.knownCached(r) {
				reorder = true
			}
			continue
		}
		if len(p.all) >= 4000 {
			continue
		}
		p.seen[r.ID] = true
		if r.Info.Height == 0 && r.Info.Title == "" {
			r.Info = releaseparse.Parse(r.Name)
		}
		p.all = append(p.all, r)
		added = append(added, r)
	}
	if p.first.IsZero() {
		p.first = time.Now()
	}
	if len(added) > 0 {
		ranked, _, err := p.svc.rankCandidates(p.ctx, p.req, p.search, p.profile, added, nil, "")
		if err != nil {
			return
		}
		p.ranked = append(p.ranked, ranked...)
	}
	if len(added) == 0 && !reorder {
		return
	}
	p.ranked = p.svc.orderCandidates(p.ranked, p.profile, p.req.Title, nil)
	p.pumpLocked()
}

// Four in flight, twelve total, with the next ranked candidate replacing a
// failed check immediately. Reposts do not consume every speculative slot.
func (p *searchPreparation) pumpLocked() {
	if p.closed {
		return
	}
	active := 0
	for _, c := range p.byID {
		select {
		case <-c.done:
		default:
			active++
		}
	}
	for _, c := range p.ranked {
		if active >= 4 || len(p.byID) >= earlyCheckLimit {
			break
		}
		if prior := p.byID[c.rel.ID]; prior != nil {
			select {
			case <-prior.done:
				if prior.result.healthy && prior.result.err == nil {
					return
				}
			default:
			}
			continue
		}
		identity := c.rel.Protocol + "|" + strings.ToLower(strings.TrimSpace(c.rel.Name)) + "|" + strconv.FormatInt(c.rel.Size, 10)
		if p.identities[identity] {
			continue
		}
		p.identities[identity] = true
		p.releases[c.rel.ID] = c.rel
		spec := core.WithBackgroundBudget(p.ctx, func(ctx context.Context, resource, source string, amount int64) error {
			return core.SpendBackground(p.ctx, resource, source, amount)
		})
		check := p.checks.start(context.WithValue(spec, speculativeHealthKey{}, true), p.svc, c.rel)
		if !core.IsBackgroundCheck(p.ctx) {
			check.retrySpeculative = func(ctx context.Context) validation { return p.svc.validateOne(ctx, c.rel) }
		}
		p.byID[c.rel.ID] = check
		active++
		go p.refillAfter(check)
	}
}

// refillAfter starts the next arrived candidate when check ends without a
// healthy result. Before, more checks started only when another indexer
// answered, so a page of failed top candidates waited out the slowest
// indexer's timeout.
func (p *searchPreparation) refillAfter(check *candidateCheck) {
	<-check.done
	if check.result.healthy && check.result.err == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pumpLocked()
}

// discoveryWindow lets concurrently dispatched indexers answer. Play then
// starts on the first healthy profile-passing candidate instead of the
// slowest indexer's HTTP timeout. A require-fallback waits up to
// criticalPathDeadline for a primary-tier hit; remaining indexers keep
// searching in the background for a better version and failover.
const (
	discoveryWindow      = 500 * time.Millisecond
	criticalPathDeadline = 4 * time.Second
)

func (p *searchPreparation) readyResults() []core.Release {
	return p.readyResultsAfter(discoveryWindow)
}

func (p *searchPreparation) readyResultsAfter(window time.Duration) []core.Release {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled || p.first.IsZero() || time.Since(p.first) < window {
		return nil
	}
	if len(p.ranked) > 1 {
		p.ranked = p.svc.orderCandidates(p.ranked, p.profile, p.req.Title, nil)
	}
	p.pumpLocked()
	margin := p.svc.resolverCfg().SourceScoreMargin
	if margin <= 0 {
		margin = defaultSourceScoreMargin
	}
	lead := -1
	for i, c := range p.ranked {
		f := p.byID[c.rel.ID]
		if f == nil {
			if lead < 0 {
				lead = i
			}
			continue
		}
		select {
		case <-f.done:
			if f.result.err != nil || !f.result.healthy {
				continue
			}
			if c.policy.Fallback > 0 && time.Since(p.first) < criticalPathDeadline {
				if lead < 0 {
					lead = i
				}
				continue
			}
			leadUncachedTorrent := lead >= 0 &&
				protocolOf(p.ranked[lead].rel) == core.ProtocolTorrent &&
				!p.svc.knownCached(p.ranked[lead].rel)
			if lead >= 0 && !(leadUncachedTorrent && p.svc.knownCached(c.rel)) && (!equivalentHedge(p.ranked[lead], c, margin) || time.Since(p.first) < rankedSourceGrace) {
				continue
			}
			preference := p.svc.resolverCfg().SourcePreference
			other := preference == PreferDebrid && protocolOf(c.rel) != core.ProtocolTorrent || preference == PreferUsenet && protocolOf(c.rel) != core.ProtocolUsenet
			if other && time.Since(p.first) < rankedSourceGrace {
				continue
			}
			return append([]core.Release(nil), p.all...)
		default:
			if lead < 0 {
				lead = i
			}
		}
	}
	return nil
}

func (p *searchPreparation) searchLive(ctx context.Context, indexer core.StreamingIndexer, req core.SearchRequest) ([]core.Release, bool, error) {
	if p.disabled {
		r, e := indexer.SearchWith(ctx, req, p.arrived)
		return r, false, e
	}
	// Detach from Play cancel so a first healthy hit can start while the
	// remaining indexers finish into the search cache (better versions and
	// failover). Close still cancels and joins via bgCtx/bgWG.
	searchCtx, cancel := context.WithCancel(p.svc.bgCtx)
	type answer struct {
		releases []core.Release
		err      error
	}
	done := make(chan answer, 1)
	p.svc.bgMu.Lock()
	if p.svc.bgClosed {
		p.svc.bgMu.Unlock()
		cancel()
		return nil, false, context.Canceled
	}
	p.svc.bgWG.Add(1)
	p.svc.bgMu.Unlock()
	go func() {
		defer p.svc.bgWG.Done()
		defer cancel()
		r, e := indexer.SearchWith(searchCtx, req, p.arrived)
		done <- answer{r, e}
		if p.partial.Load() && e == nil && len(r) > 0 {
			p.svc.rememberSearchResults(req, r)
		}
	}()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case a := <-done:
			return a.releases, false, a.err
		case <-ticker.C:
			if ready := p.readyResults(); len(ready) > 0 {
				if ctx.Err() != nil {
					cancel()
					<-done
					return nil, false, ctx.Err()
				}
				p.svc.log.Info("source discovery finished on playable candidate", "candidates", len(ready), "elapsed", time.Since(p.first))
				p.partial.Store(true)
				// SearchWith keeps running on bgCtx; the worker defers cancel.
				p.svc.keepSearchCancel(cancel)
				return ready, true, nil
			}
		case <-ctx.Done():
			cancel()
			<-done
			return nil, false, ctx.Err()
		}
	}
}

// keepSearchCancel marks cancel as owned by the background search so an
// early Play start is not a leaked context.
func (s *Service) keepSearchCancel(context.CancelFunc) {}

func (p *searchPreparation) attach(cs []candidate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range cs {
		r := cs[i].rel
		if original, ok := p.releases[r.ID]; ok && original.Name == r.Name && original.Size == r.Size && original.NZBURL == r.NZBURL && original.Protocol == r.Protocol {
			cs[i].prepared = p.byID[r.ID]
		}
	}
}
func (p *searchPreparation) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.checks.close()
}
