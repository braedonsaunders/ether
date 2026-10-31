// Package usenet is Ether's Usenet layer: an NNTP client, per-provider
// connection pools with priority scheduling and circuit breakers, and a
// multi-provider fetcher that implements core.ArticleFetcher with
// primary-first ordering and backfill from block accounts.
package usenet

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/yenc"
)

// Defaults for Options.
const (
	DefaultConnections       = 8
	DefaultBodyPipelineDepth = 4
	DefaultMinIdle           = 2
	DefaultPrefetchFraction  = 0.25
	DefaultIdleTimeout       = 90 * time.Second
	DefaultKeepAlive         = 30 * time.Second
	DefaultMissTTL           = 2 * time.Minute
	DefaultMissCacheSize     = 100_000
	DefaultRetries           = 2
	DefaultBackoffMin        = time.Second
	DefaultBackoffMax        = 2 * time.Minute
	DefaultFailThreshold     = 2
	DefaultLimitCooloff      = 5 * time.Minute
	DefaultAuthRetryMin      = 15 * time.Minute
	DefaultAuthRetryMax      = 6 * time.Hour
)

// Options tune the fetcher. Zero values take the defaults above.
type Options struct {
	// BodyPipelineDepth bounds queued BODY commands per socket (default 4,
	// maximum 16). One disables pipelining. Readahead normally uses at most two,
	// with a reserved live connection; explicit short bursts can borrow idle
	// capacity and use this depth. Prefetch, recovery probes and single-connection readahead
	// use one. Speculative batches stop growing when live work waits.
	BodyPipelineDepth int
	Logger            *slog.Logger
	// PrefetchFraction is the share of each provider's connections that
	// PriorityPrefetch requests may hold at once (at least one).
	PrefetchFraction float64
	// DialTimeout bounds connect + TLS + greeting + login.
	DialTimeout time.Duration
	// CommandTimeout bounds one command, including reading an article body.
	CommandTimeout time.Duration
	// IdleReadTimeout bounds stalled command/body reads; default 5 seconds.
	IdleReadTimeout time.Duration
	// MinIdle keeps this many primary (non-backfill) connections per
	// provider warm at all times: established in the background, held
	// past IdleTimeout and DATE-pinged so Play never pays a cold dial.
	// 0 = DefaultMinIdle; negative disables. A provider's own
	// config.UsenetProvider.MinIdle overrides it.
	MinIdle int
	// IdleTimeout closes excess connections unused for this long.
	IdleTimeout time.Duration
	// KeepAlive pings (DATE) connections idle for this long so servers do not
	// drop them; negative disables pings.
	KeepAlive time.Duration
	// MissTTL is how long a 430 from a provider is remembered per article;
	// negative disables the miss cache.
	MissTTL       time.Duration
	MissCacheSize int
	// Retries is how many extra attempts a provider gets, each on another
	// connection, after transient (network/protocol) errors.
	Retries int
	// Circuit breaker: after FailThreshold consecutive failed connects a
	// provider is skipped for BackoffMin, doubling up to BackoffMax.
	BackoffMin    time.Duration
	BackoffMax    time.Duration
	FailThreshold int
	// AuthRetryMin/Max: a provider that rejected the credentials (481/482)
	// is re-probed with one login after AuthRetryMin, doubling per failed
	// probe up to AuthRetryMax, so transient auth outages self-heal.
	AuthRetryMin time.Duration
	AuthRetryMax time.Duration
	// LimitCooloff: when a server refuses connections beyond some number
	// while others work, the pool shrinks to that number for this long.
	LimitCooloff time.Duration
	// TLSConfig is the base TLS client configuration (custom roots, e.g. in
	// tests). Nil uses the system roots.
	TLSConfig *tls.Config
	// DialContext overrides the network dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// UseGroup issues GROUP (first of the segment's groups) before BODY. Most
	// providers serve BODY <message-id> without it.
	UseGroup bool
	// MaxArticleSize caps a decoded article; 0 = yenc.DefaultMaxSize.
	MaxArticleSize int64
	// TraceProtocol emits raw NNTP status lines at debug level.
	TraceProtocol bool
}

func (o *Options) withDefaults() {
	if o.BodyPipelineDepth <= 0 {
		o.BodyPipelineDepth = DefaultBodyPipelineDepth
	}
	o.BodyPipelineDepth = min(o.BodyPipelineDepth, 16)
	if o.MinIdle == 0 {
		o.MinIdle = DefaultMinIdle
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PrefetchFraction <= 0 || o.PrefetchFraction > 1 {
		o.PrefetchFraction = DefaultPrefetchFraction
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = DefaultCommandTimeout
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	if o.KeepAlive == 0 {
		o.KeepAlive = DefaultKeepAlive
	}
	if o.MissTTL == 0 {
		o.MissTTL = DefaultMissTTL
	}
	if o.MissCacheSize <= 0 {
		o.MissCacheSize = DefaultMissCacheSize
	}
	if o.Retries < 0 {
		o.Retries = 0
	} else if o.Retries == 0 {
		o.Retries = DefaultRetries
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = DefaultBackoffMin
	}
	if o.BackoffMax < o.BackoffMin {
		o.BackoffMax = max(DefaultBackoffMax, o.BackoffMin)
	}
	if o.FailThreshold <= 0 {
		o.FailThreshold = DefaultFailThreshold
	}
	if o.LimitCooloff <= 0 {
		o.LimitCooloff = DefaultLimitCooloff
	}
	if o.AuthRetryMin <= 0 {
		o.AuthRetryMin = DefaultAuthRetryMin
	}
	if o.AuthRetryMax < o.AuthRetryMin {
		o.AuthRetryMax = max(DefaultAuthRetryMax, o.AuthRetryMin)
	}
	if o.MaxArticleSize <= 0 {
		o.MaxArticleSize = yenc.DefaultMaxSize
	}
}

// Fetcher implements core.ArticleFetcher over the owner's providers.
//
// Ordering: non-backfill providers are tried in ascending Priority; providers
// sharing a Priority form a tier and share load (the least busy is tried
// first). Only when every non-backfill provider answered 430 are backfill
// (block) providers tried, in the same way. Transient errors are retried on
// fresh connections and then on the next provider.
type Fetcher struct {
	o   Options
	log *slog.Logger

	mu      sync.RWMutex
	pools   []*pool   // every enabled provider, in config order
	retired []*pool   // removed pools with article windows still finishing
	tiers   [][]*pool // try order
	closed  bool
	// share maps a provider's configured connections to this node's part
	// of them (clustered); nil uses every configured connection.
	share func(configured int) int
	gen   atomic.Uint64
	rr    atomic.Uint64
	// minIdle is the fetcher-wide warm floor (already defaulted);
	// UpdateConfig changes it live.
	minIdle atomic.Int32

	misses *missCache
}

var _ core.ArticleFetcher = (*Fetcher)(nil)

// New builds a fetcher for providers. Connections are dialled lazily,
// except each primary provider's MinIdle warm floor, which the pool
// janitors establish (and keep) in the background.
func New(providers []config.UsenetProvider, opts Options) *Fetcher {
	opts.withDefaults()
	f := &Fetcher{
		o:      opts,
		log:    opts.Logger,
		misses: newMissCache(opts.MissTTL, opts.MissCacheSize, time.Now),
	}
	f.minIdle.Store(int32(opts.MinIdle))
	f.Update(providers)
	return f
}

// UpdateConfig applies an edited Usenet config: the provider list (see
// Update) and the MinIdle warm floor, live and without a restart.
// MinIdle 0 takes the default; negative disables the floor.
func (f *Fetcher) UpdateConfig(u config.Usenet) {
	n := u.MinIdle
	if n == 0 {
		n = DefaultMinIdle
	}
	f.minIdle.Store(int32(n))
	f.Update(u.Providers)
	// Reused pools pick up the new floor here; newly created ones read
	// f.minIdle in Update. Either way, nudge the janitor so a raised
	// floor is established now rather than at the next sweep.
	f.mu.RLock()
	pools := append([]*pool(nil), f.pools...)
	f.mu.RUnlock()
	for _, p := range pools {
		p.setGlobalMinIdle(n)
		p.maintainFloor()
	}
}

// Update swaps in a new provider list. Providers whose settings are
// unchanged keep their pool, connections, statistics and circuit-breaker
// state; changed providers get a fresh pool (which also clears an
// authentication lockout); removed or disabled ones are closed gracefully:
// queued requests move on to other providers and busy connections close
// when their current article finishes.
func (f *Fetcher) Update(providers []config.UsenetProvider) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	old := f.pools
	var next []*pool
	reused := make(map[*pool]bool)
	for _, cfg := range providers {
		if !cfg.Enabled || cfg.Host == "" {
			continue
		}
		var p *pool
		for _, op := range old {
			if op.cfg == cfg && !reused[op] {
				p = op
				break
			}
		}
		if p == nil {
			key := cmp.Or(cfg.ID, cfg.Host) + "#" + strconv.FormatUint(f.gen.Add(1), 10)
			p = newPool(key, cfg, &f.o, f.log, int(f.minIdle.Load()))
			p.setShare(shareFor(f.share, p.size))
		}
		reused[p] = true
		next = append(next, p)
	}
	// Retain only draining pools so Close can cancel windows surviving an
	// earlier provider reload. Completed generations need no retained state.
	draining := f.retired[:0]
	for _, p := range f.retired {
		if p.hasBodyWork() {
			draining = append(draining, p)
		}
	}
	clear(f.retired[len(draining):])
	f.retired = draining
	for _, p := range old {
		if !reused[p] {
			f.retired = append(f.retired, p)
		}
	}
	f.pools = next
	f.tiers = buildTiers(next)
	f.mu.Unlock()

	for _, p := range old {
		if !reused[p] {
			p.closeMode(false)
		}
	}
	f.log.Debug("usenet: providers updated", "enabled", len(next))
}

func buildTiers(pools []*pool) [][]*pool {
	sorted := slices.Clone(pools)
	slices.SortStableFunc(sorted, func(a, b *pool) int {
		if a.cfg.Backfill != b.cfg.Backfill {
			if a.cfg.Backfill {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.cfg.Priority, b.cfg.Priority)
	})
	var tiers [][]*pool
	for i, p := range sorted {
		if i > 0 {
			q := sorted[i-1]
			if q.cfg.Backfill == p.cfg.Backfill && q.cfg.Priority == p.cfg.Priority {
				tiers[len(tiers)-1] = append(tiers[len(tiers)-1], p)
				continue
			}
		}
		tiers = append(tiers, []*pool{p})
	}
	return tiers
}

// order returns the providers to try, tier by tier, least loaded first
// within a tier (ties rotate so equal providers share work).
func (f *Fetcher) order() []*pool {
	f.mu.RLock()
	tiers := f.tiers
	f.mu.RUnlock()
	var out []*pool
	rot := int(f.rr.Add(1))
	for _, tier := range tiers {
		if len(tier) == 1 {
			out = append(out, tier[0])
			continue
		}
		t := make([]*pool, len(tier))
		for i := range tier {
			t[i] = tier[(i+rot)%len(tier)]
		}
		loads := make(map[*pool]float64, len(t))
		for _, p := range t {
			s := p.sem.stats()
			w := s.Waiting[0] + s.Waiting[1] + s.Waiting[2]
			p.bodyMu.Lock()
			for _, q := range p.bodyQueue {
				w += len(q)
			}
			p.bodyMu.Unlock()
			loads[p] = float64(s.InUse+w) / float64(s.Size)
		}
		slices.SortStableFunc(t, func(a, b *pool) int { return cmp.Compare(loads[a], loads[b]) })
		out = append(out, t...)
	}
	return out
}

// Body returns the decoded payload of one article.
func (f *Fetcher) Body(ctx context.Context, messageID string, groups []string, prio core.Priority) ([]byte, error) {
	data, _, err := f.BodyPart(ctx, messageID, groups, prio)
	return data, err
}

// BodyPart returns the decoded payload and its yEnc part header. It returns
// an error wrapping core.ErrArticleMissing only when every provider able to
// answer said 430. Providers locked out after an authentication failure
// cannot answer and are not counted; providers in connection backoff are.
func (f *Fetcher) BodyPart(ctx context.Context, messageID string, groups []string, prio core.Priority) ([]byte, core.ArticlePart, error) {
	var ap core.ArticlePart
	id, err := normalizeMessageID(messageID)
	if err != nil {
		return nil, ap, err
	}
	var part *yenc.Part
	err = f.each(ctx, id, "fetch-body", func(ctx context.Context, p *pool) error {
		var e error
		if core.IsBackgroundCheck(ctx) {
			prio = core.PriorityPrefetch
			if e = core.SpendBackground(ctx, "bytes", "usenet", 2*p.o.MaxArticleSize+(64<<10)); e != nil {
				return e
			}
		}
		part, e = p.body(ctx, id, groups, prio)
		return e
	})
	if err != nil {
		return nil, ap, err
	}
	h := part.Header
	ap = core.ArticlePart{Name: h.Name, FileSize: h.Size, Part: h.Part, Total: h.Total, Begin: h.Begin, End: h.End}
	if ap.Part == 0 {
		ap.Part, ap.Total = 1, max(h.Total, 1)
	}
	return part.Data, ap, nil
}

// Stat reports whether any provider has the article. A foreground health
// check runs at live priority; a background check (availability, optimistic
// post-start) runs at prefetch. It returns (false, nil) when
// every provider able to answer said 430, and an error when that could not
// be established.
func (f *Fetcher) Stat(ctx context.Context, messageID string) (bool, error) {
	id, err := normalizeMessageID(messageID)
	if err != nil {
		return false, err
	}
	err = f.each(ctx, id, "stat", func(ctx context.Context, p *pool) error {
		ok, e := p.stat(ctx, id)
		if e == nil && !ok {
			return ErrNoSuchArticle
		}
		return e
	})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, core.ErrArticleMissing):
		return false, nil
	}
	return false, err
}

// StatStream checks a set of articles with pipelined STAT, reporting each
// article to fn as soon as its fate is decided: present=true the moment any
// provider answers 223, and present=false once every provider that could
// answer said 430 (primary tiers first, then backfill). Results flow per
// pipelined chunk as they arrive instead of waiting for the slowest batch,
// so a caller that only needs the first confirmed miss (a health check)
// can stop early: fn returning false cancels all outstanding work and
// StatStream returns nil.
//
// An article is never reported missing on the strength of provider errors:
// if some provider failed to answer for it, it stays undecided and
// StatStream returns the first such error after reporting everything that
// was decided. fn is called from one goroutine at a time, with ids exactly
// as the caller passed them.
func (f *Fetcher) StatStream(ctx context.Context, messageIDs []string, fn func(id string, present bool) bool) error {
	if len(messageIDs) == 0 {
		return nil
	}
	if fn == nil {
		return errors.New("usenet: StatStream without a result function")
	}
	type pending struct {
		caller    string
		missed    bool // at least one provider answered 430
		uncertain bool // at least one provider failed to answer
	}
	pend := make(map[string]*pending, len(messageIDs))
	var order []string // normalized ids, first-seen order
	for _, id := range messageIDs {
		normalized, err := normalizeMessageID(id)
		if err != nil {
			return err
		}
		if _, ok := pend[normalized]; !ok {
			pend[normalized] = &pending{caller: id}
			order = append(order, normalized)
		}
	}
	all := f.order()
	if len(all) == 0 {
		return ErrNoProviders
	}
	// Providers locked out by an authentication failure cannot answer and
	// are not counted, as in eachOnce: otherwise one bad login would leave
	// every missing article undecided.
	var pools []*pool
	var firstErr error
	for _, p := range all {
		if err := p.brk.allow(); errors.Is(err, ErrProviderDisabled) {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", providerLabel(p.cfg), err)
			}
			continue
		}
		pools = append(pools, p)
	}
	if len(pools) == 0 {
		return fmt.Errorf("%w: %w", ErrNoProviders, firstErr)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for poolIdx, p := range pools {
		lastPool := poolIdx == len(pools)-1
		if len(pend) == 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var ids []string
		for _, id := range order {
			if _, ok := pend[id]; ok {
				ids = append(ids, id)
			}
		}
		workers := max(1, min(4, p.target(), len(ids)))
		chunks := make([][]string, workers)
		for i, id := range ids {
			chunks[i%workers] = append(chunks[i%workers], id)
		}
		type answer struct {
			chunk  []string
			values map[string]bool
			err    error
		}
		answers := make(chan answer, len(chunks))
		launched := 0
		for _, chunk := range chunks {
			if len(chunk) == 0 {
				continue
			}
			launched++
			go func(chunk []string) {
				values, err := p.statMany(ctx, chunk)
				answers <- answer{chunk: chunk, values: values, err: err}
			}(chunk)
		}
		stopped := false
		received := 0
		for ; received < launched && !stopped; received++ {
			a := <-answers
			if a.err != nil {
				if firstErr == nil {
					firstErr = a.err
				}
				if errors.Is(a.err, ErrProviderDisabled) || errors.Is(a.err, ErrAuthFailed) {
					// Locked out mid-request (bad credentials): this
					// provider cannot answer and is not counted.
					continue
				}
				// A provider error means a negative from another provider
				// cannot prove global absence for these articles.
				for _, id := range a.chunk {
					if st, ok := pend[id]; ok {
						st.uncertain = true
					}
				}
				continue
			}
			for id, ok := range a.values {
				st, still := pend[id]
				if !still {
					continue
				}
				if !ok {
					st.missed = true
					f.misses.add(id, p.key)
					// On the last provider a 430 is final (unless an
					// earlier provider could not answer): report it now
					// rather than waiting for the slowest chunk.
					if lastPool && !st.uncertain {
						delete(pend, id)
						if !fn(st.caller, false) {
							stopped = true
							break
						}
					}
					continue
				}
				delete(pend, id)
				if !fn(st.caller, true) {
					stopped = true
					break
				}
			}
		}
		if stopped {
			cancel()
			// Workers send into a buffered channel and observe the
			// cancelled context; drain so none outlive the return.
			for ; received < launched; received++ {
				<-answers
			}
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	undecided := false
	for _, id := range order {
		st, still := pend[id]
		if !still {
			continue
		}
		if st.missed && !st.uncertain {
			if !fn(st.caller, false) {
				return nil
			}
		} else {
			undecided = true
		}
	}
	if undecided {
		if firstErr != nil {
			return firstErr
		}
		return ErrNoProviders
	}
	return nil
}

// StatMany checks a set of articles while pipelining commands over several
// connections to the first available provider. Missing articles are retried
// through the normal provider chain, preserving block-account backfill.
// Results are keyed by the message-ids exactly as given.
func (f *Fetcher) StatMany(ctx context.Context, messageIDs []string) (map[string]bool, error) {
	result := make(map[string]bool, len(messageIDs))
	if len(messageIDs) == 0 {
		return result, nil
	}
	seen := make(map[string]bool, len(messageIDs))
	err := f.StatStream(ctx, messageIDs, func(id string, present bool) bool {
		if normalized, nerr := normalizeMessageID(id); nerr == nil {
			seen[normalized] = present
		}
		return true
	})
	for _, id := range messageIDs {
		normalized, nerr := normalizeMessageID(id)
		if nerr != nil {
			return nil, nerr
		}
		result[id] = seen[normalized]
	}
	return result, err
}

// Warm dials, negotiates TLS, and authenticates idle connections before a
// playback request needs them. perProvider is clamped to each provider pool.
func (f *Fetcher) Warm(ctx context.Context, perProvider int) error {
	if perProvider < 1 {
		return nil
	}
	f.mu.RLock()
	pools := append([]*pool(nil), f.pools...)
	f.mu.RUnlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for _, p := range pools {
		wg.Add(1)
		go func(p *pool) {
			defer wg.Done()
			if err := p.warm(ctx, perProvider); err != nil {
				mu.Lock()
				if first == nil {
					first = fmt.Errorf("usenet: warm %s: %w", providerLabel(p.cfg), err)
				}
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	return first
}

// each runs op against providers in order until one succeeds, and turns the
// collected failures into the right error.
func (f *Fetcher) each(ctx context.Context, id, kind string, op func(context.Context, *pool) error) error {
	var swapped bool
	err := f.eachOnce(ctx, id, kind, op, &swapped)
	if swapped && err != nil && !errors.Is(err, core.ErrArticleMissing) && ctxErr(ctx) == nil {
		// A pool was replaced by Update while we were using it: go again
		// over the current providers.
		err = f.eachOnce(ctx, id, kind, op, &swapped)
	}
	// Only after every provider was tried may a BODY wait for recovery. Healthy
	// backfill remains immediate; missing/corrupt/auth failures never enter this
	// loop. One fixed budget spans all probes, so a broken provider cannot turn
	// retries into an unbounded wave. STAT/health probes keep their fast failure.
	if kind == "fetch-body" && errors.Is(err, ErrProviderBackoff) && ctxErr(ctx) == nil {
		recovery, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		lastBackoff := err
		for pass := 0; pass < 8 && errors.Is(err, ErrProviderBackoff) && recovery.Err() == nil; pass++ {
			lastBackoff = err
			knownMissing := f.misses.get(id)
			var candidate *pool
			var soonest time.Duration
			for _, p := range f.order() {
				if slices.Contains(knownMissing, p.key) {
					continue
				}
				if delay, ok := p.brk.recoveryDelay(); ok && (candidate == nil || delay < soonest) {
					candidate, soonest = p, delay
				}
			}
			if candidate == nil || candidate.brk.waitReady(recovery) != nil {
				break
			}
			err = f.eachOnce(recovery, id, kind, op, &swapped)
		}
		if ctxErr(ctx) != nil {
			return ctxErr(ctx)
		}
		if recovery.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
			return lastBackoff
		}
	}
	return err
}

// eachOnce is one pass over the providers. It sets *swapped when a pool
// turned out to be closed by a concurrent Update.
func (f *Fetcher) eachOnce(ctx context.Context, id, kind string, op func(context.Context, *pool) error, swapped *bool) error {
	pools := f.order()
	if len(pools) == 0 {
		return ErrNoProviders
	}
	known := f.misses.get(id)
	var (
		answered, missing int
		lastErr, corrupt  error
		// lastMissing is the last provider's "no such article" reply, kept
		// so the final missing error preserves the original NNTP code
		// (430 vs the 451 takedown some providers answer): both map to
		// ErrNoSuchArticle for the retry/failover decision, but the code
		// must stay observable in errors/logs/health reasons.
		lastMissing error
	)
	for _, p := range pools {
		if slices.Contains(known, p.key) {
			answered++
			missing++
			continue
		}
		if err := p.brk.allow(); errors.Is(err, ErrProviderDisabled) {
			lastErr = fmt.Errorf("%s: %w", providerLabel(p.cfg), err)
			continue
		}
		answered++
	attempts:
		for attempt := 0; attempt <= f.o.Retries; attempt++ {
			err := op(ctx, p)
			if err == nil {
				if (kind == "body" || kind == "fetch-body") && p.cfg.Backfill && missing > 0 {
					p.backfillSaves.Add(1)
				}
				return nil
			}
			if cerr := ctxErr(ctx); cerr != nil {
				return cerr
			}
			switch {
			case errors.Is(err, core.ErrBudgetExhausted):
				return err
			case errors.Is(err, ErrNoSuchArticle):
				missing++
				lastMissing = err
				f.misses.add(id, p.key)
				break attempts
			case errors.Is(err, ErrInvalidMessageID):
				return err
			case errors.Is(err, ErrCorrupt):
				corrupt = fmt.Errorf("%s: %w", providerLabel(p.cfg), err)
				break attempts
			case errors.Is(err, ErrProviderDisabled), errors.Is(err, ErrAuthFailed):
				// Locked out mid-request (bad credentials): cannot answer.
				answered--
				lastErr = fmt.Errorf("%s: %w", providerLabel(p.cfg), err)
				break attempts
			case errors.Is(err, ErrPoolClosed):
				*swapped = true
				lastErr = fmt.Errorf("%s: %w", providerLabel(p.cfg), err)
				break attempts
			case errors.Is(err, ErrProviderBackoff):
				lastErr = fmt.Errorf("%s: %w", providerLabel(p.cfg), err)
				break attempts
			default:
				lastErr = fmt.Errorf("%s: %w", providerLabel(p.cfg), err)
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break attempts
				}
				f.log.Debug("usenet: transient error; retrying", "provider", providerLabel(p.cfg), "msgid", id, "attempt", attempt+1, "err", err)
			}
		}
	}
	switch {
	case answered > 0 && missing == answered:
		if lastMissing != nil {
			// Decision unchanged (still ErrArticleMissing); the wrapped
			// reply keeps its NNTP code reachable via errors.As
			// (*ProtocolError) and visible in the error string.
			return fmt.Errorf("%s: %w: %w", id, core.ErrArticleMissing, lastMissing)
		}
		return fmt.Errorf("%s: %w", id, core.ErrArticleMissing)
	case answered == 0:
		return fmt.Errorf("%w: %w", ErrNoProviders, lastErr)
	case corrupt != nil && lastErr == nil:
		return corrupt
	case corrupt != nil:
		return errors.Join(corrupt, lastErr)
	}
	return lastErr
}

// Stats returns a snapshot per enabled provider, in configuration order.
func (f *Fetcher) Stats() []ProviderStats {
	f.mu.RLock()
	pools := f.pools
	f.mu.RUnlock()
	out := make([]ProviderStats, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.stats())
	}
	return out
}

// MaxConnections is the sum of all enabled primary providers' connection
// limits on this node: the configured counts, or this node's shares of
// them when SetConnectionShare divides them among cluster nodes.
func (f *Fetcher) MaxConnections() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	n := 0
	for _, p := range f.pools {
		if !p.cfg.Backfill {
			n += p.target()
		}
	}
	return n
}

// SetConnectionShare divides provider connection limits among cluster
// nodes: share receives a provider's configured connection count (the
// account-wide limit) and returns this node's part of it, clamped to
// 1..configured. Nil restores the configured counts (a standalone server).
// Pools resize in place and gracefully (see pool.setShare); providers
// added later by Update get their share as they are created. Call it again
// whenever membership changes.
func (f *Fetcher) SetConnectionShare(share func(configured int) int) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.share = share
	pools := slices.Clone(f.pools)
	f.mu.Unlock()
	for _, p := range pools {
		p.setShare(shareFor(share, p.size))
	}
}

func shareFor(share func(int) int, configured int) int {
	if share == nil {
		return 0
	}
	return min(max(share(configured), 1), configured)
}

// Close closes every pool. Further requests fail with ErrNoProviders.
func (f *Fetcher) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	pools := append(slices.Clone(f.pools), f.retired...)
	f.pools, f.tiers, f.retired = nil, nil, nil
	f.mu.Unlock()
	for _, p := range pools {
		p.close()
	}
	return nil
}

// checkBodyDrainMax is the largest announced part size whose remainder
// CheckBodyPart drains to keep the connection reusable. Bigger articles
// are abandoned by closing the connection instead: draining ~1 MiB would
// delay the health verdict on the Play critical path, while the redial
// happens off that path (the warm-floor janitor replaces the connection
// in the background). BenchmarkCheckBody measures both strategies.
var checkBodyDrainMax int64 = 128 << 10

// CheckBody verifies actual BODY availability without downloading a full
// article. See CheckBodyPart.
func (f *Fetcher) CheckBody(ctx context.Context, messageID string, groups []string, prio core.Priority) error {
	return f.CheckBodyPart(ctx, messageID, groups, prio, core.ArticlePart{})
}

// CheckBodyPart verifies that BODY really serves the expected yEnc part:
// providers often answer 223 to STAT while BODY returns a 451/430 DMCA
// takedown. It reads only the =ybegin/=ypart header lines (bounded, so a
// malicious unterminated line cannot consume memory), checks them against
// want where want is known (zero fields are unchecked), and then either
// drains a small remainder so the connection stays reusable or closes the
// connection so unread bytes cannot poison reuse. It returns an error
// wrapping core.ErrArticleMissing when every provider answered 430/451,
// and ErrCorrupt when a provider served something that is not the
// expected yEnc part.
func (f *Fetcher) CheckBodyPart(ctx context.Context, messageID string, groups []string, prio core.Priority, want core.ArticlePart) error {
	id, err := normalizeMessageID(messageID)
	if err != nil {
		return err
	}
	return f.each(ctx, id, "body", func(ctx context.Context, p *pool) error {
		if core.IsBackgroundCheck(ctx) {
			prio = core.PriorityPrefetch
			if err := core.SpendBackground(ctx, "bytes", "usenet", 2*p.o.MaxArticleSize+(64<<10)); err != nil {
				return err
			}
		}
		err := p.withConn(ctx, prio, func(c *Conn) error {
			if p.o.UseGroup && len(groups) > 0 && !slices.Contains(groups, c.CurrentGroup()) {
				if err := c.Group(ctx, groups[0]); err != nil && !errors.Is(err, ErrNoSuchGroup) {
					return err
				}
			}
			reader, err := c.Body(ctx, id)
			if err == nil {
				err = probeBodyHeader(c, reader, p.o.MaxArticleSize, want)
			}
			if errors.Is(err, ErrNoSuchArticle) {
				p.bodyMisses.Add(1)
				var pe *ProtocolError
				if errors.As(err, &pe) && pe.Code == 451 {
					p.bodyTakedowns.Add(1)
				}
			}
			return err
		})
		if err != nil {
			return err
		}
		p.brk.success()
		return nil
	})
}

// probeBodyHeader validates the yEnc header at the start of an open BODY
// response and leaves the connection either drained (reusable) or closed.
func probeBodyHeader(c *Conn, reader *BodyReader, maxPart int64, want core.ArticlePart) error {
	hdr, err := readYencHeader(c)
	if err != nil {
		// Not the expected article (or the wire failed): never reuse a
		// connection whose body position is unknown.
		_ = c.Close()
		reader.finish(nil)
		return err
	}
	if err := validateYencHeader(hdr, maxPart, want); err != nil {
		_ = c.Close()
		reader.finish(nil)
		return err
	}
	if hdr.PartSize() <= checkBodyDrainMax {
		// Small remainder: drain it (bounded by a generous estimate of
		// the encoded size) so this connection serves the next request.
		if derr := drainBodyBounded(c, reader, 2*hdr.PartSize()+(64<<10)); derr != nil {
			return derr
		}
		return nil
	}
	_ = c.Close()
	reader.finish(nil)
	return nil
}

// readBoundedLine reads one CRLF line straight from the connection buffer,
// dot-unstuffed, reporting the "." terminator. A line longer than the
// buffer is an error rather than an unbounded allocation.
func readBoundedLine(c *Conn) (line []byte, terminator bool, err error) {
	raw, err := c.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, false, fmt.Errorf("%w: overlong line in BODY probe", ErrCorrupt)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, false, err
	}
	line = bytes.TrimRight(raw, "\r\n")
	if len(line) == 1 && line[0] == '.' {
		return nil, true, nil
	}
	if len(line) > 0 && line[0] == '.' {
		line = line[1:]
	}
	return line, false, nil
}

// readYencHeader scans to the =ybegin line (skipping a bounded preamble)
// and parses the following =ypart line of multipart posts.
func readYencHeader(c *Conn) (yenc.Header, error) {
	var hdr yenc.Header
	for i := 0; i < 32; i++ {
		line, term, err := readBoundedLine(c)
		if err != nil {
			return hdr, err
		}
		if term {
			return hdr, fmt.Errorf("%w: empty article body", ErrCorrupt)
		}
		if !bytes.HasPrefix(line, []byte("=ybegin ")) {
			continue
		}
		hdr, err = yenc.ParseBegin(line)
		if err != nil {
			return hdr, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		if hdr.Part <= 0 {
			return hdr, nil
		}
		line, term, err = readBoundedLine(c)
		if err != nil {
			return hdr, err
		}
		if term || !bytes.HasPrefix(line, []byte("=ypart ")) {
			return hdr, fmt.Errorf("%w: multipart article without =ypart", ErrCorrupt)
		}
		if err := yenc.ParsePart(line, &hdr); err != nil {
			return hdr, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		return hdr, nil
	}
	return hdr, fmt.Errorf("%w: no =ybegin header", ErrCorrupt)
}

func validateYencHeader(hdr yenc.Header, maxPart int64, want core.ArticlePart) error {
	if hdr.PartSize() < 0 || (maxPart > 0 && hdr.PartSize() > maxPart) {
		return fmt.Errorf("%w: part size %d out of range", ErrCorrupt, hdr.PartSize())
	}
	part := hdr.Part
	if part == 0 {
		part = 1 // single-part post
	}
	switch {
	case want.Part > 0 && part != want.Part:
		return fmt.Errorf("%w: yEnc part %d, want %d", ErrCorrupt, part, want.Part)
	case want.Total > 0 && hdr.Total > 0 && hdr.Total != want.Total:
		return fmt.Errorf("%w: yEnc total %d, want %d", ErrCorrupt, hdr.Total, want.Total)
	case want.FileSize > 0 && hdr.Size != want.FileSize:
		return fmt.Errorf("%w: yEnc file size %d, want %d", ErrCorrupt, hdr.Size, want.FileSize)
	case want.Begin > 0 && hdr.Begin != want.Begin:
		return fmt.Errorf("%w: yEnc begin %d, want %d", ErrCorrupt, hdr.Begin, want.Begin)
	case want.End > 0 && hdr.End != want.End:
		return fmt.Errorf("%w: yEnc end %d, want %d", ErrCorrupt, hdr.End, want.End)
	}
	return nil
}

// drainBodyBounded reads a BODY remainder to its terminator, line by line,
// giving up (by closing the connection, without failing the probe) when
// the remainder is far larger than announced or a line overruns the
// buffer. A transport error mid-body fails the probe: the article is not
// fully there.
func drainBodyBounded(c *Conn, reader *BodyReader, maxBytes int64) error {
	var n int64
	for {
		raw, err := c.r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			_ = c.Close()
			reader.finish(nil)
			return nil
		}
		if err != nil {
			reader.finish(err)
			return reader.ioErr
		}
		n += int64(len(raw))
		if n > maxBytes {
			_ = c.Close()
			reader.finish(nil)
			return nil
		}
		if line := bytes.TrimRight(raw, "\r\n"); len(line) == 1 && line[0] == '.' {
			reader.finish(nil)
			return reader.ioErr
		}
	}
}
