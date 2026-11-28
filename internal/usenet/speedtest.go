package usenet

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/sockerr"
	"github.com/braedonsaunders/ether/internal/speedtest"
)

type speedArticle struct {
	id     string
	groups []string
}

func speedArticles(ctx context.Context, doc *nzb.NZB) []speedArticle {
	// Use distinct articles across the release and cap even season packs.
	const maxArticles = 32768
	var articles []speedArticle
	seen := make(map[string]struct{})
	for _, file := range doc.Files {
		if ctx.Err() != nil {
			return nil
		}
		if file == nil {
			continue
		}
		for _, seg := range file.Segments {
			if seg.MessageID == "" {
				continue
			}
			if _, found := seen[seg.MessageID]; found {
				continue
			}
			seen[seg.MessageID] = struct{}{}
			articles = append(articles, speedArticle{seg.MessageID, file.Groups})
			if len(articles) == maxArticles {
				return articles
			}
		}
	}
	return articles
}

type speedReads struct {
	start, end                                    time.Duration
	bytes                                         int64
	admitted, used, opened, refused, hits, misses int
	failedReaders                                 int
	spare, yielded                                bool
}

// SpeedTest measures uncached BODY reads on exactly one saved provider.
func (f *Fetcher) SpeedTest(ctx context.Context, id string, docs []*nzb.NZB, playback bool) speedtest.Result {
	var p *pool
	f.mu.RLock()
	for _, candidate := range f.pools {
		if candidate.cfg.ID == id {
			p = candidate
			break
		}
	}
	f.mu.RUnlock()
	r := speedtest.Result{Status: "unavailable", SpareOnly: playback}
	if p == nil {
		return r
	}
	if p.brk.idleAllowed() != nil {
		r.Status = "paused"
		return r
	}
	start := time.Now()
	testCtx, stop := context.WithTimeout(ctx, speedtest.Duration)
	defer stop()
	addDiagnostics := func(run speedReads) {
		r.Connections = max(r.Connections, run.used)
		r.Opened += run.opened
		r.Refused += run.refused
		r.Misses += run.misses
		r.SpareOnly = r.SpareOnly || run.spare || run.yielded
		if run.admitted > 0 {
			r.Seconds = time.Since(start).Seconds()
		}
	}

	for _, doc := range docs {
		if testCtx.Err() != nil {
			break
		}
		if doc == nil {
			continue
		}
		articles := speedArticles(testCtx, doc)
		if len(articles) == 0 {
			continue
		}
		// Verify a spread of articles with BODY. STAT can return 223 for
		// articles the provider won't deliver. Keep probed IDs out of the
		// transfer so the rate doesn't depend on warming them at the server.
		n := max(1, min(8, len(articles)/2))
		probe := make([]speedArticle, 0, n)
		rest := articles[:0]
		for i, a := range articles {
			if len(probe) < n && i == len(probe)*len(articles)/n {
				probe = append(probe, a)
			} else {
				rest = append(rest, a)
			}
		}
		run := p.readSpeedArticles(testCtx, probe, 1, playback, n/2+1)
		addDiagnostics(run)
		switch {
		case run.yielded || run.admitted == 0:
			r.Status = "busy"
			return r
		case run.misses > n/2:
			r.Status = "sample_missing"
			continue
		case run.hits+run.misses < n || run.hits == 0:
			r.Status = "unavailable"
			return r
		}
		if len(rest) > 0 {
			run = p.readSpeedArticles(testCtx, rest, p.sem.stats().Size, playback, 0)
			addDiagnostics(run)
		}
		switch {
		case run.misses > run.hits:
			r.Status = "sample_missing"
		case run.admitted == 0 || (run.yielded && run.hits == 0):
			r.Status = "busy"
			return r
		case run.hits == 0 || run.failedReaders == run.used:
			r.Status = "unavailable"
			return r
		default:
			r.Bytes, r.Connections = run.bytes, run.used
			r.Status = "unavailable"
			r.MeasureTransfer(run.end - run.start)
			return r
		}
	}
	return r
}

func (p *pool) readSpeedArticles(ctx context.Context, articles []speedArticle, workers int, playback bool, missLimit int) speedReads {
	var run speedReads
	var wg sync.WaitGroup
	var next atomic.Int64
	goRead := make(chan struct{})
	results := make([]speedReads, min(workers, len(articles)))
	state := p.sem.stats()
	reserve := 0
	if playback || state.InUse > 0 {
		reserve = 1
		run.spare = true
	}

	for i := range results {
		worker, cancel := context.WithCancel(ctx)
		if !p.sem.trySpeedTest(&cancel, reserve) {
			cancel()
			state := p.sem.stats()
			run.spare = run.spare || state.InUse > run.admitted ||
				state.Waiting != [numPriorities]int{}
			break
		}
		run.admitted++
		wg.Go(func() {
			defer cancel()
			<-goRead
			local := &results[i]
			defer func() {
				local.yielded = worker.Err() == context.Canceled && ctx.Err() == nil
			}()
			c, fresh, err := p.checkout(worker)
			if err != nil {
				p.sem.doneSpeedTest(&cancel)
				p.sem.Release(core.PriorityLive)
				if speedConnectionRefused(err) {
					local.refused++
				}
				return
			}
			defer func() {
				p.sem.doneSpeedTest(&cancel)
				p.put(c, core.PriorityLive)
			}()
			if fresh {
				local.opened++
			}
			for worker.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= len(articles) {
					return
				}
				if p.brk.idleAllowed() != nil {
					local.failedReaders = local.used
					return
				}
				local.used = 1
				n, started, err := p.readSpeedBody(worker, c, articles[i])
				if n > 0 && (err == nil || errors.Is(ctxErr(worker), context.DeadlineExceeded)) {
					if local.bytes == 0 {
						local.start = started
					}
					local.end = speedTransferTime()
					local.bytes += n
				}
				switch {
				case err == nil:
					if n > 0 {
						local.hits++
					}
					p.brk.success()
				case errors.Is(err, ErrNoSuchArticle):
					local.misses++
					p.failed(err, !fresh, worker)
					if local.misses == missLimit {
						return
					}
				default:
					if speedConnectionRefused(err) {
						local.refused++
					}
					if ctxErr(worker) == nil {
						local.failedReaders++
					}
					p.failed(err, !fresh, worker)
					return
				}
				fresh = false
			}
		})
	}
	close(goRead)
	wg.Wait()
	for _, local := range results[:run.admitted] {
		run.yielded = run.yielded || local.yielded
		run.used += local.used
		run.opened += local.opened
		run.refused += local.refused
		run.hits += local.hits
		run.misses += local.misses
		run.failedReaders += local.failedReaders
		if local.bytes > 0 {
			if run.bytes == 0 || local.start < run.start {
				run.start = local.start
			}
			run.end = max(run.end, local.end)
			run.bytes += local.bytes
		}
	}
	return run
}

func speedConnectionRefused(err error) bool {
	return errors.Is(err, ErrServiceUnavailable) || sockerr.IsConnRefused(err)
}

func (p *pool) readSpeedBody(ctx context.Context, c *Conn, a speedArticle) (int64, time.Duration, error) {
	if p.o.UseGroup && len(a.groups) > 0 && !slices.Contains(a.groups, c.CurrentGroup()) {
		if err := c.Group(ctx, a.groups[0]); err != nil && !errors.Is(err, ErrNoSuchGroup) {
			return 0, 0, err
		}
	}
	reader, err := c.Body(ctx, a.id)
	if err != nil {
		return 0, 0, err
	}
	started := speedTransferTime()
	n, err := io.Copy(io.Discard, reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	return n, started, err
}
