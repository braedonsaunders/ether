package resolver

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Source routing: among releases of equivalent quality, the source that
// starts playback fastest goes first.
//
// Quality is never traded for speed. Routing only reorders candidates in
// the leader's quality class (sameQuality: height, HDR kind, source class,
// bitrate floor and audio language) whose score is within a margin of the
// leader's. Within that group:
//
//   - "auto" (the default) prefers the source with the lower measured
//     startup time once both have enough samples and differ by a clear
//     margin. Until then, popular titles prefer cached debrid: a cached
//     torrent is one HTTPS range read from a CDN, while Usenet needs an NZB
//     grab, an article health check and archive parsing.
//   - "usenet" and "debrid" always prefer that source.
//
// Only torrents known to be cached on a debrid service count as fast; an
// uncached torrent would cost a provider round trip just to be skipped.
// The best release of the other source is then placed second, so ranked
// validation (which starts the top two checks together) hedges across
// sources, and failover alternates include both.
//
// Startup is measured as NZB/payload fetch plus a health check that really
// ran, for each validation that passed: for Usenet that covers the indexer
// grab, article checks and first articles; for debrid the provider calls
// and the first block. Verified-cache answers are left out.

const (
	PreferAuto   = "auto"
	PreferUsenet = "usenet"
	PreferDebrid = "debrid"

	defaultSourceScoreMargin = 25
	// minStartupSamples per source before measurements decide.
	minStartupSamples = 5
	// minStartupGain is the smallest measured difference that reorders.
	minStartupGain = 250 * time.Millisecond
	startupAlpha   = 0.2

	// A title is popular enough to default to cached debrid when TMDB rates
	// it this popular or with this many votes, or its best torrent has this
	// many seeders (episodes often carry no popularity of their own).
	popularPopularity = 50
	popularVotes      = 1000
	popularSeeders    = 50
)

// SourceSignals is what routing needs from the debrid service.
type SourceSignals interface {
	// KnownCached reports whether a debrid provider is known to hold the
	// torrent (from bulk cache checks or earlier plays), without a request.
	KnownCached(infoHash string) bool
}

// SourceStartup is a source's measured startup time.
type SourceStartup struct {
	Protocol string        `json:"protocol"` // "usenet" or "torrent"
	Average  time.Duration `json:"average"`  // exponentially weighted
	Last     time.Duration `json:"last"`
	Samples  int64         `json:"samples"`
}

type routingState struct {
	mu      sync.Mutex
	signals SourceSignals
	startup map[string]*SourceStartup
}

// SetSourceSignals wires the debrid cache knowledge; nil disables it.
func (s *Service) SetSourceSignals(sig SourceSignals) {
	s.route.mu.Lock()
	s.route.signals = sig
	s.route.mu.Unlock()
}

// SourceStartups returns the measured startup per source.
func (s *Service) SourceStartups() []SourceStartup {
	s.route.mu.Lock()
	defer s.route.mu.Unlock()
	out := []SourceStartup{}
	for _, p := range []string{core.ProtocolUsenet, core.ProtocolTorrent} {
		if st := s.route.startup[p]; st != nil {
			out = append(out, *st)
		}
	}
	return out
}

// protocolOf is a release's routing source. An addon's HTTP stream is
// read like a debrid file (HTTPS ranges from a CDN), so it routes with
// debrid; it is never known to be cached, so only an explicit debrid
// preference moves it first.
func protocolOf(rel core.Release) string {
	if rel.Protocol == core.ProtocolTorrent || rel.Protocol == core.ProtocolHTTP {
		return core.ProtocolTorrent
	}
	return core.ProtocolUsenet
}

func sourceName(protocol string) string {
	if protocol == core.ProtocolTorrent {
		return "debrid"
	}
	return "usenet"
}

func (s *Service) noteStartup(rel core.Release, d time.Duration) {
	if d <= 0 {
		return
	}
	p := protocolOf(rel)
	s.route.mu.Lock()
	defer s.route.mu.Unlock()
	if s.route.startup == nil {
		s.route.startup = map[string]*SourceStartup{}
	}
	st := s.route.startup[p]
	if st == nil {
		st = &SourceStartup{Protocol: p, Average: d}
		s.route.startup[p] = st
	} else {
		st.Average = time.Duration(float64(st.Average)*(1-startupAlpha) + float64(d)*startupAlpha)
	}
	st.Last = d
	st.Samples++
}

func (s *Service) startupOf(p string) (time.Duration, bool) {
	s.route.mu.Lock()
	defer s.route.mu.Unlock()
	st := s.route.startup[p]
	if st == nil || st.Samples < minStartupSamples {
		return 0, false
	}
	return st.Average, true
}

func (s *Service) knownCached(rel core.Release) bool {
	if rel.Protocol != core.ProtocolTorrent || rel.InfoHash == "" {
		return false
	}
	s.route.mu.Lock()
	sig := s.route.signals
	s.route.mu.Unlock()
	return sig != nil && sig.KnownCached(rel.InfoHash)
}

// routeSources reorders ranked candidates (best first) by source, as
// described above. It never changes which quality tier leads.
func (s *Service) routeSources(cands []candidate, t *core.Title, trace *Trace) []candidate {
	if len(cands) < 2 {
		return cands
	}
	rcfg := s.resolverCfg()
	mode := strings.ToLower(strings.TrimSpace(rcfg.SourcePreference))
	explicit := mode == PreferDebrid || mode == PreferUsenet
	preferredProtocol := core.ProtocolUsenet
	if mode == PreferDebrid {
		preferredProtocol = core.ProtocolTorrent
	}
	// Automatic routing reuses a verified source. An explicit owner change
	// still takes effect for equal-quality releases, even after prior plays.
	if cands[0].verified && (!explicit || protocolOf(cands[0].rel) == preferredProtocol) {
		return cands
	}
	margin := rcfg.SourceScoreMargin
	if margin <= 0 {
		margin = defaultSourceScoreMargin
	}
	lead := cands[0]
	// Verified candidates are promoted ahead of the score order, so equal-tier
	// alternatives need not be adjacent to the leader. Explicit source choices
	// must consider the whole eligible tier instead of stopping at a verified
	// release of another quality.
	var group []candidate
	var indexes []int
	for i, c := range cands {
		eligible := c.bandwidth >= lead.bandwidth && (!c.verified || explicit) && sameQuality(c, lead) && c.rel.Score >= lead.rel.Score-margin
		if !eligible {
			if !explicit {
				break
			}
			continue
		}
		group = append(group, c)
		indexes = append(indexes, i)
	}
	hasUsenet, hasTorrent := false, false
	for _, c := range group {
		if protocolOf(c.rel) == core.ProtocolTorrent {
			hasTorrent = true
		} else {
			hasUsenet = true
		}
	}
	if hasTorrent && !hasUsenet {
		// Availability still matters when only torrents are eligible. The
		// search cache check may already know a ready equivalent, while a
		// slightly higher-scored descriptor has no hash yet. Promote the
		// ready copy before preparation so unrelated descriptor discovery
		// does not delay its validation. Keep unknown copies as fallbacks.
		for _, i := range indexes {
			if !s.knownCached(cands[i].rel) || !equivalentHedge(lead, cands[i], margin) {
				continue
			}
			if i == 0 {
				return cands
			}
			out := make([]candidate, 0, len(cands))
			out = append(out, cands[i])
			out = append(out, cands[:i]...)
			out = append(out, cands[i+1:]...)
			if trace != nil && out[0].trace >= 0 && out[0].trace < len(trace.Candidates) {
				trace.Candidates[out[0].trace].Route = "ready equivalent: cached on debrid"
			}
			return out
		}
		return cands
	}
	if !hasTorrent {
		return cands
	}

	var want, why string
	switch mode {
	case PreferUsenet:
		want, why = core.ProtocolUsenet, "owner prefers usenet"
	case PreferDebrid:
		want, why = core.ProtocolTorrent, "owner prefers debrid"
	default:
		want, why = s.autoSource(group, t)
	}
	if want == "" {
		return cands
	}
	fast := func(c candidate) bool {
		if protocolOf(c.rel) != want {
			return false
		}
		// Under auto, only a known-cached torrent is fast; an owner who
		// prefers debrid accepts the provider round trip.
		return want != core.ProtocolTorrent || mode == PreferDebrid || s.knownCached(c.rel)
	}
	first := -1
	// An explicit debrid preference still means ready-to-stream first.
	// Unknown availability is a fallback, not a reason to spend the whole
	// validation budget before a known-cached equivalent gets a chance.
	if want == core.ProtocolTorrent {
		for i, c := range group {
			if s.knownCached(c.rel) {
				first = indexes[i]
				break
			}
		}
	}
	for i, c := range group {
		if first < 0 && fast(c) {
			first = indexes[i]
			break
		}
	}
	if first < 0 {
		return cands
	}
	out := make([]candidate, 0, len(cands))
	out = append(out, cands[first])
	// Hedge: the best of the other source second, so ranked validation
	// checks both sources at once.
	other := -1
	for i, c := range group {
		if indexes[i] != first && protocolOf(c.rel) != want {
			other = indexes[i]
			break
		}
	}
	if other >= 0 {
		out = append(out, cands[other])
	}
	for i, c := range cands {
		if i != first && i != other {
			out = append(out, c)
		}
	}
	if trace != nil {
		note := "faster source: " + why
		if protocolOf(out[0].rel) == core.ProtocolTorrent && s.knownCached(out[0].rel) {
			note += " (cached on debrid)"
		}
		if out[0].trace >= 0 && out[0].trace < len(trace.Candidates) {
			trace.Candidates[out[0].trace].Route = note
		}
		if other >= 0 && out[1].trace >= 0 && out[1].trace < len(trace.Candidates) {
			trace.Candidates[out[1].trace].Route = "hedge: checked alongside on " + sourceName(protocolOf(out[1].rel))
		}
		trace.Routing = sourceName(want) + " first: " + why
	}
	return out
}

// autoSource picks the preferred protocol for "auto": measured startup
// first, then popularity. "" means no preference.
func (s *Service) autoSource(group []candidate, t *core.Title) (string, string) {
	u, uok := s.startupOf(core.ProtocolUsenet)
	d, dok := s.startupOf(core.ProtocolTorrent)
	if uok && dok {
		switch {
		case u-d >= minStartupGain:
			return core.ProtocolTorrent, fmt.Sprintf("measured debrid %s vs usenet %s", roundMs(d), roundMs(u))
		case d-u >= minStartupGain:
			return core.ProtocolUsenet, fmt.Sprintf("measured usenet %s vs debrid %s", roundMs(u), roundMs(d))
		}
		// Too close to call: fall through to popularity.
	}
	if popular(group, t) {
		return core.ProtocolTorrent, "popular title"
	}
	return "", ""
}

func popular(group []candidate, t *core.Title) bool {
	if t != nil && (t.Popularity >= popularPopularity || t.VoteCount >= popularVotes) {
		return true
	}
	for _, c := range group {
		if c.rel.Protocol == core.ProtocolTorrent && c.rel.Seeders >= popularSeeders {
			return true
		}
	}
	return false
}

func roundMs(d time.Duration) string {
	return d.Round(10 * time.Millisecond).String()
}
