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
// account. Freshness is the only other admission test: an edge not seen
// recently is not worth a probe.
type recentEdges struct {
	mu sync.Mutex
	at map[string]map[string]time.Time // provider account -> edge host -> block completion
}

const (
	recentEdgeTTL   = 10 * time.Minute
	recentEdgeTries = 3 // remembered edges one reroute wave probes, freshest first
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
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].at.After(fresh[j].at) })
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

// probeSample is what one probe asks an edge for: enough to try the route,
// leaving sustained delivery to the real read.
const probeSample = 64

// probeLink spends one small range read asking an edge whether it serves
// this link, at the offset the file just failed on. The edge must answer
// with a 206 whose Content-Range is exactly bytes off-end of a file of
// size and deliver the whole sample: a 204, a 200 HTML error page, a 206
// truncated short or a 206 for another offset prove nothing. The probe
// follows no redirects, so the answer comes from the approved edge itself
// and the link's token never hops to a host the provider never approved.
func (e *Engine) probeLink(ctx context.Context, raw string, off, size int64) bool {
	end := min(off+probeSample-1, size-1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
	req.Header.Set("User-Agent", httpclient.UserAgent)
	resp, err := e.hcReplay.Do(req) // the probe, like the read, answers no redirects
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent ||
		!probeRangeAnswers(resp.Header.Get("Content-Range"), off, end, size) {
		return false
	}
	want := end - off + 1
	n, err := io.CopyN(io.Discard, resp.Body, want)
	return err == nil && n == want
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

// resumeOnAnotherEdge replays the file's current link on an edge the
// engine has served completed blocks from for the same provider account
// (recentEdges). While the provider pins the failed host into every
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
	p, gen := f.payload()
	off := min(max(start, 0), f.size-1)
	changed := true // the first pass always runs; later ones only once the link moved under us
	for round := 0; round < rerouteLinks && changed; round++ {
		changed = false
		approver, ok := cdnReplayApproval(f.e.providerImpl(p.Provider))
		if p.URL == "" || p.IsDirect() || p.IsPluginProvider() || !ok {
			return false, false
		}
		hosts := f.e.recent.freshest(p.Provider, urlHost(failed), f.e.backoff.down, f.e.now())
		if len(hosts) > recentEdgeTries {
			hosts = hosts[:recentEdgeTries] // probe the freshest few, not the whole map
		}
		for _, host := range hosts {
			if !approver.approvesCDNEdge(p.URL, host) {
				continue // never hand the token to a host outside this provider's CDN
			}
			u := withHost(p.URL, host)
			if u == "" || u == p.URL || !f.e.probeLink(ctx, u, off, f.size) {
				continue
			}
			f.mu.Lock()
			if f.gen != gen { // a refresh or adopt moved the link while the probe ran
				p, gen = f.pl, f.gen
				f.mu.Unlock()
				if usable(p.URL) { // whoever moved the file took it to a live route: they won
					return true, false
				}
				// The next request must be vetted and minted by the NEW
				// snapshot's provider: an old account's edge may not
				// accept the new account's token, and only the new
				// account's ledger may be probed with it at all.
				changed = true
				break // rebuild approver and candidate list, then re-run the wave
			}
			f.pl.URL = u
			f.pl.OnReplayEdge = true // the read that follows must not hop off this edge
			f.gen++
			f.mu.Unlock()
			return true, true
		}
	}
	return false, false
}
