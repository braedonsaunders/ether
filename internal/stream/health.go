package stream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Check implements core.HealthChecker. It parses the NZB, inspects the
// archive headers of the main video (rejecting compressed, solid and
// unsupported encryption and releases without a playable file) and STATs a
// spread sample of the main video's articles in parallel: the first, the
// last and evenly spaced ones across every volume.
//
// healthy=false comes with an error saying why: one wrapping
// core.ErrArticleMissing (sampled articles are gone or the NZB is
// incomplete), one of the package's rejection errors (see IsRejection), or a
// transient error (network, context).
//
// When the release carries a small PAR2 index it is read alongside, and the
// first article of the main video (or first archive volume) is checked
// against the recorded length, first-16 KiB MD5 and covered slice
// checksums; a mismatch is reported as unhealthy with an error wrapping
// ErrPar2Mismatch (a rejection). A missing or unusable PAR2 is ignored.
//
// The index built here is kept, so an Open of the same NZB right after a
// successful Check does not repeat the header fetches.
func (e *Engine) Check(ctx context.Context, nzbData []byte) (healthy bool, err error) {
	return e.CheckRelease(ctx, "", nzbData)
}

// CheckRelease is Check for a known release ID: the PAR2 index and the
// archive heads it reads are kept in the segment cache under that ID, so
// the Open that follows a passed check finds them there as well as in the
// hot buffer. Implements core.ReleaseHealthChecker.
type healthWorkKey struct{}

func (e *Engine) CheckRelease(ctx context.Context, releaseID string, nzbData []byte) (healthy bool, err error) {
	if e.repairOptions().Enabled {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	ctx = context.WithValue(ctx, healthWorkKey{}, true)
	fo := fetchOpts{cacheID: releaseID}
	defer func() {
		if err != nil && IsRejection(err) {
			err = fmt.Errorf("%w: %w", core.ErrReleaseRejected, err)
		}
	}()
	trace := HealthTrace{}
	started := time.Now()
	defer func() {
		e.healthMu.Lock()
		e.healthTrace = trace
		e.healthMu.Unlock()
		e.log.Debug("release health completed", "release", releaseID, "healthy", healthy, "err", err,
			"total", time.Since(started), "parse", trace.Parse, "plan", trace.Plan,
			"sample", trace.STAT, "body", trace.Body, "samples", trace.Samples,
			"bodyProbes", trace.BodyProbes, "bodyOK", trace.BodyOK, "failReason", trace.FailReason)
	}()
	if err := e.ctx.Err(); err != nil {
		return false, ErrClosed
	}
	parseStart := time.Now()
	rel, err := e.acquire(nzbData)
	trace.Parse = time.Since(parseStart)
	if err != nil {
		return false, err
	}
	defer e.release(rel)

	b := &builder{e: e, rel: rel, prio: healthPriority(ctx)}
	cands := b.candidates(rel.files)

	// STAT the likely main candidate while its headers are inspected.
	type statResult struct {
		files    []*postedFile
		out      sampleOutcome
		duration time.Duration
	}
	statCh := make(chan statResult, 1)
	sctx, cancelStats := context.WithCancel(ctx)
	defer cancelStats()
	var sampleOnce sync.Once
	startSample := func(files []*postedFile) {
		sampleOnce.Do(func() {
			go func() {
				started := time.Now()
				out := e.statSample(sctx, files, func(ref slotRef) bool { return e.repairHealthSample(sctx, rel, ref, fo) })
				statCh <- statResult{files: files, out: out, duration: time.Since(started)}
			}()
		})
	}
	earlySample := len(cands) > 0 && !b.shouldProbe(cands)
	if earlySample {
		startSample(cands[0].files)
	}
	record := func(res statResult) {
		trace.STAT, trace.Samples = res.duration, res.out.total
		trace.Body, trace.BodyProbes, trace.BodyOK = res.out.bodyDuration, res.out.bodyProbes, res.out.bodyOK
		trace.FailFast, trace.FailReason = res.out.failFast, res.out.reason
	}

	// Read the PAR2 index (if small) alongside, so the first volume can be
	// checked against it as soon as the plan is ready.
	par2Done := make(chan struct{})
	if idx := rel.par2IndexFile(); idx != nil {
		go func() {
			defer close(par2Done)
			rel.loadPar2(sctx, healthPriority(sctx), fo, idx)
		}()
	} else {
		close(par2Done)
	}

	type planned struct {
		p       *plan
		err     error
		elapsed time.Duration
	}
	planCh := make(chan planned, 1)
	go func() {
		started := time.Now()
		p, err := rel.ensurePlanWithCandidate(sctx, healthPriority(sctx), releaseID, startSample)
		// Identification can reject before announcing any candidate. Always
		// settle the sample channel so cancellation cleanup can join it.
		startSample(nil)
		planCh <- planned{p, err, time.Since(started)}
	}()
	var ready planned
	var res statResult
	gotStats, gotPlan := false, false
	defer func() {
		cancelStats()
		if !gotStats {
			reason := trace.FailReason
			record(<-statCh)
			if reason != "" {
				trace.FailReason = reason
			}
		}
		if !gotPlan {
			trace.Plan = (<-planCh).elapsed
		}
		<-par2Done
	}()
	par2Fail := func(err error) {
		if errors.Is(err, ErrPar2Mismatch) {
			trace.FailReason = par2Reason(err)
		}
	}
	for !gotStats || !gotPlan {
		select {
		case <-ctx.Done():
			cancelStats()
			return false, ctx.Err()
		case res = <-statCh:
			gotStats = true
			record(res)
			// Newly identified obfuscated candidates may still fall through
			// to another archive if the first contains no video. Its sample
			// must not reject the eventual candidate before planning settles.
			// Likewise, a transient STAT error must not cancel planning before
			// an archive header check can report a permanent rejection.
			if (earlySample && res.out.missing > 0) || (gotPlan && sameFiles(res.files, ready.p.vols) && (res.out.err != nil || res.out.missing > 0)) {
				cancelStats()
				if res.out.err != nil {
					return false, res.out.err
				}
				return false, fmt.Errorf("stream: sampled BODY or STAT unavailable: %w", core.ErrArticleMissing)
			}
		case ready = <-planCh:
			gotPlan = true
			trace.Plan = ready.elapsed
			if ready.err != nil {
				par2Fail(ready.err)
				return false, ready.err
			}
			// Beside the STAT/BODY probes still running: the first
			// volume's head (fetched by the plan) against PAR2.
			if err := rel.verifyPar2(ctx, ready.p, healthPriority(ctx), fo, true); err != nil {
				par2Fail(err)
				return false, err
			}
		}
	}
	p := ready.p
	if !sameFiles(res.files, p.vols) {
		started := time.Now()
		out := e.statSample(ctx, p.vols, func(ref slotRef) bool { return e.repairHealthSample(ctx, rel, ref, fo) })
		res = statResult{files: p.vols, out: out, duration: time.Since(started)}
	}
	record(res)
	if res.out.err != nil {
		return false, res.out.err
	}
	if res.out.missing > 0 {
		return false, fmt.Errorf("stream: %d of %d sampled articles of %s missing: %w", res.out.missing, res.out.total, p.name, core.ErrArticleMissing)
	}
	return true, nil
}

func sameFiles(a, b []*postedFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// healthSample picks n slots spread over the concatenated parts of files:
// always the first and the last, the rest evenly spaced.
func healthSample(files []*postedFile, n int) []slotRef {
	var total int
	for _, pf := range files {
		total += pf.numSlots()
	}
	if total == 0 {
		return nil
	}
	if n < 2 {
		n = 2
	}
	at := func(g int) slotRef {
		for _, pf := range files {
			if g < pf.numSlots() {
				return slotRef{pf, g}
			}
			g -= pf.numSlots()
		}
		last := files[len(files)-1]
		return slotRef{last, last.numSlots() - 1}
	}
	if total <= n {
		out := make([]slotRef, 0, total)
		for g := 0; g < total; g++ {
			out = append(out, at(g))
		}
		return out
	}
	out := make([]slotRef, 0, n)
	prev := -1
	for j := 0; j < n; j++ {
		g := int((int64(j)*int64(total-1) + int64(n-1)/2) / int64(n-1))
		if g == prev {
			continue
		}
		prev = g
		out = append(out, at(g))
	}
	return out
}

// healthHeadSlots are the leading articles every health check confirms on
// top of the spread sample: playback reads them within its first seconds,
// so a gap there fails the start (or forces an immediate failover) however
// healthy the rest of the post is. Twelve articles are about 9 MB, and STAT
// is pipelined, so they cost one round trip.
const healthHeadSlots = 12

// withHead adds the first k slots of files to a sample, without repeats.
func withHead(refs []slotRef, files []*postedFile, k int) []slotRef {
	seen := map[slotRef]bool{}
	for _, r := range refs {
		seen[r] = true
	}
	for _, pf := range files {
		for i := 0; i < pf.numSlots() && k > 0; i++ {
			k--
			ref := slotRef{pf, i}
			if !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
		if k == 0 {
			break
		}
	}
	return refs
}

type slotRef struct {
	pf   *postedFile
	slot int
}

// Optional fetcher capabilities the health check uses when present. The
// production fetcher (internal/usenet) implements all of them; plain
// core.ArticleFetcher implementations fall back to per-article Stat and a
// full Body fetch for probes.
type bodyPartChecker interface {
	CheckBodyPart(context.Context, string, []string, core.Priority, core.ArticlePart) error
}
type bodyChecker interface {
	CheckBody(context.Context, string, []string, core.Priority) error
}
type statStreamer interface {
	StatStream(context.Context, []string, func(id string, present bool) bool) error
}

// sampleOutcome is the verdict of sampling one candidate's articles.
type sampleOutcome struct {
	missing      int
	total        int
	bodyProbes   int
	bodyOK       int
	bodyDuration time.Duration // slowest BODY probe
	failFast     bool          // bailed on the first confirmed miss
	reason       string        // which probe confirmed the miss
	err          error
}

// expectedPart is what the yEnc header of slot's article must say, so far
// as the index already knows it: the part number and total always, the
// file size and byte range once learned from a fetched header.
func (pf *postedFile) expectedPart(slot int) core.ArticlePart {
	want := core.ArticlePart{Part: slot + 1, Total: pf.numSlots()}
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	if pf.known {
		want.FileSize = pf.size
	}
	if slot >= 0 && slot < len(pf.slots) && pf.slots[slot].known {
		// Slots are [begin,end) 0-based; yEnc is 1-based inclusive.
		want.Begin = pf.slots[slot].begin + 1
		want.End = pf.slots[slot].end
	}
	return want
}

// bodyProbeSlots picks which samples get a real BODY (yEnc header) probe:
// one or two mid-file segments, where DMCA takedowns hide behind STAT 223.
func bodyProbeSlots(n int) map[int]bool {
	switch {
	case n >= 4:
		return map[int]bool{n / 3: true, n * 2 / 3: true}
	case n == 3:
		return map[int]bool{1: true}
	}
	return nil
}

// statSample checks the health sample of files: BODY probes on the
// mid-file samples run concurrently with pipelined STATs of the rest, and
// the first article confirmed missing on every provider tier ends the
// check immediately — outstanding STAT and BODY work is cancelled rather
// than awaited.
func (e *Engine) statSample(parent context.Context, files []*postedFile, repairs ...func(slotRef) bool) (out sampleOutcome) {
	refs := healthSample(files, e.opts.HealthSamples)
	// BODY probes stay on the spread sample; the head is STAT-only.
	probes := bodyProbeSlots(len(refs))
	refs = withHead(refs, files, healthHeadSlots)
	out.total = len(refs)
	if len(refs) == 0 {
		return out
	}
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	type event struct {
		ref      slotRef
		body     bool
		statDone bool // the STAT stream ended; err explains undecided ids
		missing  bool
		id       string
		err      error
		dur      time.Duration
	}
	events := make(chan event, len(refs)+1)

	var statIDs []string
	for i, ref := range refs {
		id := ref.pf.msgID(ref.slot)
		body := probes[i]
		switch {
		case id == "":
			events <- event{body: body, missing: true, ref: ref}
		case body:
			out.bodyProbes++
			wg.Add(1)
			go func(ref slotRef, id string) {
				defer wg.Done()
				started := time.Now()
				err := e.probeBody(ctx, ref, id)
				events <- event{body: true, missing: isMissing(err), id: id, err: err, dur: time.Since(started)}
			}(ref, id)
		default:
			statIDs = append(statIDs, id)
		}
	}
	if streamer, ok := e.fetcher.(statStreamer); ok && len(statIDs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := streamer.StatStream(ctx, statIDs, func(id string, present bool) bool {
				events <- event{missing: !present, id: id}
				return ctx.Err() == nil
			})
			events <- event{statDone: true, err: err}
		}()
	} else {
		jobs := make(chan string, len(statIDs))
		for _, id := range statIDs {
			jobs <- id
		}
		close(jobs)
		for i := 0; i < min(e.opts.HealthParallel, len(statIDs)); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case id, ok := <-jobs:
						if !ok {
							return
						}
						present, err := e.fetcher.Stat(ctx, id)
						events <- event{missing: (!present && err == nil) || isMissing(err), id: id, err: err}
					}
				}
			}()
		}
	}

	needStat := len(statIDs)
	statReceived, decided := 0, 0
	var statErr error
	for decided < len(refs) {
		select {
		case <-parent.Done():
			out.err = parent.Err()
			return out
		case ev := <-events:
			if ev.statDone {
				// Ids never reported are undecided (a provider errored);
				// they count as decided-with-error, never as missing.
				statErr = ev.err
				decided += needStat - statReceived
				continue
			}
			decided++
			if ev.body {
				out.bodyDuration = max(out.bodyDuration, ev.dur)
				if ev.err == nil && !ev.missing {
					out.bodyOK++
				}
			} else {
				statReceived++
			}
			ref := ev.ref
			if ref.pf == nil && (ev.missing || repairErasure(ev.err)) {
				for _, r := range refs {
					if r.pf.msgID(r.slot) == ev.id {
						ref = r
						break
					}
				}
			}
			// A repost of the part (Segment.Alternates) serves it before
			// PAR2 repair is needed: the sample is present after all.
			if ev.missing && ref.pf != nil && len(ref.pf.alternates(ref.slot)) > 0 {
				if e.alternatePresent(ctx, ref, ev.body) {
					continue
				}
				if parent.Err() != nil {
					out.err = parent.Err()
					return out
				}
			}
			if (ev.missing || repairErasure(ev.err)) && len(repairs) > 0 && e.repairOptions().Enabled {
				if ref.pf != nil && repairs[0](ref) {
					continue
				}
				if parent.Err() != nil {
					out.err = parent.Err()
					return out
				}
			}
			if ev.missing {
				out.err = nil // a confirmed miss takes precedence over another sample's timeout
				out.missing = 1
				out.failFast = decided < len(refs)
				kind := "STAT"
				if ev.body {
					kind = "BODY"
				}
				if ev.id == "" {
					out.reason = "segment absent from NZB"
				} else if ev.err != nil {
					// Keep the underlying reply in the reason: a 451
					// takedown and a plain 430 both read as "missing"
					// for the verdict, but the NNTP code in the error
					// is the only place they remain distinguishable.
					out.reason = fmt.Sprintf("%s missing on %s: %v", kind, ev.id, ev.err)
				} else {
					out.reason = fmt.Sprintf("%s missing on %s", kind, ev.id)
				}
				return out
			}
			if ev.err != nil && parent.Err() == nil {
				out.err = ev.err
				// Another sample may still prove the release is missing.
				// Preserve this transient error, but let a confirmed miss win.
			}
		}
	}
	if statErr != nil && parent.Err() == nil {
		out.err = statErr
	}
	return out
}

// alternatePresent reports whether a repost of ref's part is available,
// checked the same way the primary was: a BODY probe (whose yEnc header
// must match the slot) or a STAT.
func (e *Engine) alternatePresent(ctx context.Context, ref slotRef, body bool) bool {
	for _, alt := range ref.pf.alternates(ref.slot) {
		if body {
			if e.probeBody(ctx, ref, alt) == nil {
				return true
			}
		} else if present, err := e.fetcher.Stat(ctx, alt); err == nil && present {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	return false
}

// healthPriority leaves a pre-play check on the live lane and demotes a
// background validation or optimistic post-start check to prefetch.
func healthPriority(ctx context.Context) core.Priority {
	if core.IsBackgroundCheck(ctx) {
		return core.PriorityPrefetch
	}
	return core.PriorityLive
}

// probeBody runs one BODY probe, preferring the yEnc-header check that
// validates the part against the NZB.
func (e *Engine) probeBody(ctx context.Context, ref slotRef, id string) error {
	prio := healthPriority(ctx)
	if checker, ok := e.fetcher.(bodyPartChecker); ok {
		return checker.CheckBodyPart(ctx, id, ref.pf.groups, prio, ref.pf.expectedPart(ref.slot))
	}
	if checker, ok := e.fetcher.(bodyChecker); ok {
		return checker.CheckBody(ctx, id, ref.pf.groups, prio)
	}
	_, err := e.fetcher.Body(ctx, id, ref.pf.groups, prio)
	return err
}
