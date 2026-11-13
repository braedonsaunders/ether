package debrid

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/httpclient"
)

// recentEdges remembers which CDN edges each provider account has recently
// served completed blocks from. Providers regenerate links onto the node
// they pinned (issue #396), and while that node dies behind a maintenance
// wall every fresh link repeats the same dead host: an API answer is no
// longer a new route. A link's path and token still stand on any edge of
// the same provider (observed on TorBox 2026-11-09), so an edge that
// delivered blocks is a spare route for a file whose own edge died. The
// evidence belongs to the account that saw it: one provider's edge says
// nothing about another's, and a signed link's token must not leave its
// account. Recent successes are preferred. Older provider-issued
// authorities are retained separately as unproven candidates and require
// the same strict probe before adoption.
type recentEdges struct {
	mu sync.Mutex
	at map[string]map[string]time.Time // provider account -> edge host -> block completion
}

const (
	recentEdgeTTL     = 10 * time.Minute
	edgeProbeBudget   = 2 * time.Second // one stalled edge must not consume the whole wave
	edgeProbeCooldown = 30 * time.Second
	recentEdgeTries   = 4 // probes one generation-snapshot spends, freshest first; every
	// probe counts toward the cap whatever its verdict, so the cap bounds
	// requests where the wall clock cannot (fast answers of every class).
	// Four reaches a healthy candidate behind three dribbled ones, and an
	// all-degraded fleet ends under the wave's 10 s budget (4×2 s) with
	// the parent context still live to adopt under. A generation change
	// mid-wave starts the tries counter afresh in ordinary replay (the
	// rebuild loop predates this cap); a discovery wave instead ends at the
	// change, so for the hunt the bound holds for one degraded generation.
)

// record notes that host delivered a complete block for this provider
// account just now. Only completed reads are evidence: a response that
// never delivered a block — a 403 turning the token away, headers nobody
// read — must not earn a replay of the token in turn.
func (r *recentEdges) record(provider, host string, now time.Time) {
	if provider == "" || host == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at == nil {
		r.at = map[string]map[string]time.Time{}
	}
	edges := r.at[provider]
	if edges == nil {
		edges = map[string]time.Time{}
		r.at[provider] = edges
	}
	edges[host] = now // pruning waits for freshest, which walks the map anyway
}

// freshest lists the hosts recorded for provider, freshest first, dropping
// exclude, expired entries and hosts the backoff marks down. Hosts are the
// same keys hostBackoff records (req.URL.Host).
func (r *recentEdges) freshest(provider, exclude string, down func(string) bool, now time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	type seen struct {
		host string
		at   time.Time
	}
	fresh := make([]seen, 0, len(r.at[provider]))
	for k, t := range r.at[provider] {
		if now.Sub(t) > recentEdgeTTL {
			delete(r.at[provider], k) // this walk is the ledger's one prune
			continue
		}
		if k == exclude {
			continue
		}
		if down != nil && down(k) {
			continue
		}
		fresh = append(fresh, seen{k, t})
	}
	sort.Slice(fresh, func(i, j int) bool {
		if fresh[i].at.Equal(fresh[j].at) {
			return fresh[i].host < fresh[j].host
		}
		return fresh[i].at.After(fresh[j].at)
	})
	out := make([]string, 0, len(fresh))
	for _, s := range fresh {
		out = append(out, s.host)
	}
	return out
}

// servedBy is the edge that answered a range for one provider account: the
// evidence recentEdges keeps when a block completes. It is captured with
// the request, because the file's current payload may move while the
// block is read.
type servedBy struct {
	provider string
	host     string // req URL host of the response (its final hop)
}

// replayKey marks a media read whose link rides a remembered CDN edge
// (Payload.OnReplayEdge). send answers the mark with the engine's
// redirect-refusing clients.
type replayKey struct{}

func withReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, replayKey{}, true)
}

func onReplayEdge(ctx context.Context) bool {
	v, _ := ctx.Value(replayKey{}).(bool)
	return v
}

// noRedirect clones a client to answer redirects instead of following
// them. Go's client sends the outgoing URL as a Referer when it follows a
// hop, and a replayed link's URL carries its signed token: refusing the
// hop keeps the token inside the approved CDN.
func noRedirect(hc *http.Client) *http.Client {
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// withHost replays raw's path and query (where a link's token lives) on
// another host — the portability that frees a file from a pinned dead edge.
func withHost(raw, host string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || host == "" {
		return ""
	}
	u.Host = host
	return u.String()
}

// probeSample is what one probe asks an edge for: big enough that one
// probe budget measures flow, not just a willingness to answer headers.
// A drained edge delivers a 64-byte sample instantly and then dribbles
// the real read (observed on TorBox 2026-11-12/13 at ~0.1-0.6 MB/s), so
// against edgeProbeBudget this sets an acceptance floor near 1 MB/s.
// The floor is coarse on purpose and bounds one completion, not a
// measured body rate: setup (DNS, TCP, TLS, first byte) shares the
// budget with the body, so an edge that starts too slowly to leave the
// budget for its sample is rejected alongside the stalled ones — and an
// edge that passes has completed a sample, never proven remux
// capacity. A healthy edge answers the same sample in milliseconds.
const probeSample = 2 << 20

// probeVerdict is what one probe says about one edge, in the evidence it
// actually produced: what the sample proved, not how the request ended.
type probeVerdict uint8

const (
	// probeRejected proved nothing usable: no valid 206, a Content-Range
	// that is not exactly the promised range, or fewer than the eligible
	// first bytes delivered.
	probeRejected probeVerdict = iota
	// probeDegraded is old main's evidence and no more: valid headers
	// and the eligible bytes, but the sample never completed — the budget
	// expired while the body was arriving (a dribble, or mere silence
	// after the first bytes: this verdict never claims flow was proven),
	// or EOF truncated the promised range. A degraded edge is a route
	// last — adopted only when the wave's candidates proved no flow.
	probeDegraded
	// probeApproved completed the whole sample inside the budget: the
	// route flowed.
	probeApproved
)

// probeEligible is the body evidence main's 64-byte probe always demanded
// before approving: a degraded verdict may not rest on less, so even a
// candidate that dies mid-sample must have delivered at least this much
// of exactly the promised range. Capped by the sample itself for a
// valid short tail.
const probeEligible int64 = 64

// probeLink spends one bounded range read asking an edge whether it serves
// this link, at the offset the file just failed on. The edge must answer
// with a 206 whose Content-Range is exactly bytes off-end of a file of
// size: a 204, a 200 HTML error page, a 206 truncated short or a 206 for
// another offset prove nothing. On top of that verdict the sample size
// separates a route that flowed (probeApproved) from one that merely
// answered: eligible bytes that never completed (budget expired mid-
// arrival, or EOF truncation) are probeDegraded — main's old evidence,
// good enough to replay on when nothing better was found, never good
// enough to prefer. At the end
// of the file the sample is capped at the remaining bytes, and the floor
// scales with them: a few-KiB tail may dribble in below the nominal rate
// and must not be rejected for it, because the sample there is the last
// thing left to read. The probe follows no redirects, so the answer comes
// from the approved edge itself and the link's token never hops to a host
// the provider never approved.
func (e *Engine) probeLink(ctx context.Context, raw string, off, size int64) probeVerdict {
	end := min(off+probeSample-1, size-1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return probeRejected
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	req.Header.Set("User-Agent", httpclient.UserAgent)
	resp, err := e.hcReplay.Do(req) // the probe, like the read, answers no redirects
	if err != nil {
		return probeRejected
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent ||
		!probeRangeAnswers(resp.Header.Get("Content-Range"), off, end, size) {
		return probeRejected
	}
	want := end - off + 1
	n, err := io.CopyN(io.Discard, resp.Body, want)
	switch {
	case err == nil && n == want:
		return probeApproved
	case n >= min(want, probeEligible) && (ctx.Err() != nil || err == io.EOF):
		// The budget expired with the body arriving, or EOF cut the
		// promised range short after eligible bytes: compatible route,
		// unproven flow. A non-EOF read error (protocol reset) after
		// eligibility is rejected — a broken pipe is not a dribble.
		return probeDegraded
	default:
		return probeRejected
	}
}

// probeRangeAnswers reports whether header v promises exactly bytes
// off-end of a file of size ("bytes off-end/size"). Where
// checkContentRange tolerates a missing header for a real read, a probe is
// the evidence a route will be taken on: it takes no chances.
func probeRangeAnswers(v string, off, end, size int64) bool {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "bytes"))
	rng, total, ok := strings.Cut(v, "/")
	if !ok {
		return false
	}
	startS, endS, ok := strings.Cut(strings.TrimSpace(rng), "-")
	if !ok {
		return false
	}
	a, err1 := strconv.ParseInt(strings.TrimSpace(startS), 10, 64)
	b, err2 := strconv.ParseInt(strings.TrimSpace(endS), 10, 64)
	t, err3 := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	return err1 == nil && err2 == nil && err3 == nil && a == off && b == end && t == size
}

// providerImpl returns the running provider implementation behind a
// payload's provider id (nil while the provider is not active).
func (e *Engine) providerImpl(id string) Provider {
	if e.svc == nil || id == "" {
		return nil
	}
	if en := e.svc.entryByID(id); en != nil {
		return en.p
	}
	return nil
}

// cdnReplayApproval reports whether a provider lets the engine replay its
// links on remembered CDN edges (edgePortableLinks) and returns the
// authority that vetoes candidate hosts (cdnEdgeApprover). A provider that
// opts into one contract without the other does not replay at all: only
// the approver keeps a signed token inside the provider's own CDN.
func cdnReplayApproval(impl Provider) (cdnEdgeApprover, bool) {
	portable, ok := impl.(edgePortableLinks)
	if !ok || !portable.edgePortableLinks() {
		return nil, false
	}
	approver, ok := impl.(cdnEdgeApprover)
	if !ok {
		return nil, false
	}
	return approver, true
}

// resumeOnAnotherEdge replays the file's current link onto a candidate
// edge: recent completed-block hosts (recentEdges) are preferred,
// followed by persisted provider-issued authorities from the same account
// (knownCDNHosts). While the provider pins the failed host into every
// regenerated link, no API answer moves the file — but the link's path and
// token stand on another edge, which is a new route the file can take
// without the provider's help. Only providers that opt in
// (edgePortableLinks) replay, and the provider approves a candidate
// authority (cdnEdgeApprover) before the probe ever hands it the token.
//
// It reports whether the file now stands on a live route and whether this
// helper installed it. A refresh or adopt that moved the link to another
// host while a probe ran wins: that route is checked with the same usable
// predicate the rest of the wave uses, and the win is reported without
// claiming the replay. A refresh that left the file on the failed host
// discards the stale probe and re-runs the wave from that snapshot — a
// fresh link from another account carries another provider's opt-in,
// approver and ledger, so nothing from the old snapshot may mint the next
// request. Within the wave's budget. On adoption the generation bumps the
// way a refresh bumps it and the payload records that it now rides a
// remembered edge (Payload.OnReplayEdge): the media read that follows will
// refuse redirects, keeping the token inside the approved CDN.
func (f *remoteFile) resumeOnAnotherEdge(ctx context.Context, failed string, usable func(string) bool, start int64) (moved, replayed bool) {
	return f.rerouteWave(ctx, failed, usable, start, nil)
}

// rerouteWave is the replay wave; a non-nil visit map selects DISCOVERY
// mode: candidates already visited in this exploration are skipped
// without spending attempts, every probe records its candidate, and only
// a flowing (approved) route is adopted — a discovery wave that merely
// dribbles ends empty-handed, leaving the degraded route in place.
func (f *remoteFile) rerouteWave(ctx context.Context, failed string, usable func(string) bool, start int64, visit map[string]bool) (moved, replayed bool) {
	p, gen := f.payload()
	off := min(max(start, 0), f.size-1)
	changed := true // the first pass always runs; later ones only once the link moved under us
	for round := 0; round < rerouteLinks && changed; round++ {
		changed = false
		fallback := "" // first candidate whose probe earned old main's evidence, kept for last resort
		approver, ok := cdnReplayApproval(f.e.providerImpl(p.Provider))
		if p.URL == "" || p.IsDirect() || p.IsPluginProvider() || !ok {
			return false, false
		}
		hosts := f.e.recent.freshest(p.Provider, urlHost(failed), f.e.backoff.down, f.e.now())
		hosts = append(hosts, f.e.svc.knownCDNHosts(ctx, p.Provider)...)
		seen := make(map[string]bool)
		unique := make([]string, 0, len(hosts))
		for _, host := range hosts {
			if !seen[host] {
				seen[host] = true
				unique = append(unique, host)
			}
		}
		hosts = unique
		cursorKey := p.Provider
		f.mu.Lock()
		cursor := ""
		if f.edgeCursorKey == cursorKey {
			cursor = f.edgeCursorHost
		}
		f.mu.Unlock()
		// A fast refusal consumes a probe too. Continue beyond the last
		// attempted host, even after a token refresh, so early failures cannot hide a
		// healthy candidate later in the list on every recovery wave.
		for i, host := range hosts {
			if host == cursor {
				hosts = append(append([]string(nil), hosts[i+1:]...), hosts[:i+1]...)
				break
			}
		}
		tries := 0
		for _, host := range hosts {
			if ctx.Err() != nil {
				return false, false
			}
			if host == urlHost(failed) || f.e.backoff.down(host) {
				continue
			}
			if !approver.approvesCDNEdge(p.URL, host) {
				continue // never hand the token to a host outside this provider's CDN
			}
			u := withHost(p.URL, host)
			if u == "" || u == p.URL {
				continue
			}
			key := p.Provider + "|" + u
			f.mu.Lock()
			now := f.e.now()
			for k, until := range f.edgeProbeAfter {
				if !now.Before(until) {
					delete(f.edgeProbeAfter, k)
				}
			}
			wait := now.Before(f.edgeProbeAfter[key])
			f.mu.Unlock()
			if wait {
				continue
			}
			if visit != nil && visit[key] {
				continue // already probed in this exploration: discovery must progress, not re-kiss
			}
			if tries >= recentEdgeTries {
				break
			}
			tries++
			f.mu.Lock()
			f.edgeCursorKey, f.edgeCursorHost = cursorKey, host
			f.mu.Unlock()
			pctx, cancel := context.WithTimeout(ctx, edgeProbeBudget)
			verdict := f.e.probeLink(pctx, u, off, f.size)
			timedOut := pctx.Err() != nil && ctx.Err() == nil
			cancel()
			f.mu.Lock()
			if ctx.Err() != nil {
				f.mu.Unlock()
				return false, false
			}
			if f.gen != gen { // a refresh or adopt moved the link while the probe ran
				if visit != nil {
					f.mu.Unlock()
					return false, false // a generation change ends the old exploration: the hunt does not re-probe on someone else's payload
				}
				p, gen = f.pl, f.gen
				f.mu.Unlock()
				if usable(p.URL) { // whoever moved the file took it to a live route: they won
					return true, false
				}
				// The next request must be vetted and minted by the NEW
				// snapshot's provider: an old account's edge may not
				// accept the new account's token, and only the new
				// account's ledger may be probed with it at all. A probe
				// that never answered bytes must notice the move too:
				// the next candidate would be minted from the old token.
				changed = true
				break // rebuild approver and candidate list, then re-run the wave
			}
			if verdict != probeApproved {
				if visit != nil {
					visit[key] = true
				}
				if f.edgeProbeAfter == nil {
					f.edgeProbeAfter = make(map[string]time.Time)
				}
				if verdict == probeDegraded {
					// Old main's evidence: remembered as the wave's last
					// resort, and cooled down like a timeout so the next
					// wave advances instead of re-kissing the dribbler.
					if fallback == "" {
						fallback = u
					}
					if len(f.edgeProbeAfter) < knownEdgeLimit {
						f.edgeProbeAfter[key] = f.e.now().Add(edgeProbeCooldown)
					}
				} else if timedOut && len(f.edgeProbeAfter) < knownEdgeLimit {
					f.edgeProbeAfter[key] = f.e.now().Add(edgeProbeCooldown)
				}
				f.mu.Unlock()
				continue // same snapshot, no flowing candidate yet: try the next
			}
			if visit != nil && f.closed {
				f.mu.Unlock()
				return false, false // a hunt does not move a closed file: drop the verdict
			}
			f.pl.URL = u
			f.pl.OnReplayEdge = true // the read that follows must not hop off this edge
			f.routeDegraded = false  // a flowed route outvotes any degraded one, from a replay or a hunt
			f.gen++
			f.mu.Unlock()
			return true, true
		}
		// Out of candidates or attempts without one flowing answer: an
		// edge that earned old main's evidence is still a route — but
		// only while this wave is still valid to hand it to. An expired
		// parent adopts nothing, exactly as a strict verdict would not,
		// and a snapshot that moved under the wave owns the file now.
		if visit == nil && fallback != "" && ctx.Err() == nil {
			f.mu.Lock()
			adopt := f.gen == gen
			if adopt {
				f.pl.URL = fallback
				f.pl.OnReplayEdge = true
				f.routeDegraded = true // flow unproven: block deliveries will drive a bounded hunt
				f.discoverLeft = routeDiscoveryWaves
				f.discovered = make(map[string]bool)
				f.degradedFrom = failed // the hunt excludes the host that failed the file, as this wave did
				f.gen++
			}
			f.mu.Unlock()
			if adopt {
				return true, true
			}
		}
	}
	return false, false
}
