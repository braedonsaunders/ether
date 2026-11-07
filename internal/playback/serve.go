package playback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var (
	// errDirectReadDeadline marks an active range that delivered no bytes
	// for the full direct-play read budget.
	errDirectReadDeadline = errors.New("playback: direct source stalled")
	// errRetired: the generation was swapped out between two reads.
	errRetired = errors.New("playback: source retired")
	// errEndResponse: a failover happened mid-response in a container that
	// cannot be spliced; the response ends early and the client re-requests.
	errEndResponse = errors.New("playback: response ended for failover")
	// errShortSource: the (failed-over) source ended before the range did.
	errShortSource = errors.New("playback: source shorter than requested range")
)

// ServeRange serves the session's media to a client (direct play) with
// Range and If-Range support. Offsets are in the coordinates of the file the
// session started with; after a failover they are mapped by timestamp into
// the current release. When articles go missing mid-response the session
// fails over: an MPEG-TS response continues from the new release at the same
// moment; other containers require a new playback decision before the client
// can read a replacement release.
func (m *Manager) ServeRange(w http.ResponseWriter, r *http.Request, sessionID string) {
	s := m.lookup(sessionID)
	if s == nil {
		http.Error(w, "unknown playback session", http.StatusNotFound)
		return
	}
	s.activeReads.Add(1)
	defer func() {
		s.activeReads.Add(-1)
		s.touch(m.now())
	}()
	s.touch(m.now())
	s.markDirectPlay()
	if !s.directRangeCompatible() {
		// The client still holds the original file's container headers and
		// byte coordinates. A remux handoff needs a new playback decision;
		// retrying this URL must never return bytes from a different file.
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "source changed; restart playback", http.StatusConflict)
		return
	}
	w = newStallWriter(w, m.opts.WriteStallTimeout)
	s.mu.Lock()
	rep := representation{
		size:                  s.virtual.Size,
		etag:                  s.etag,
		modTime:               s.modTime,
		contentType:           s.contentType,
		deferStatusOnZeroRead: true,
	}
	plexPart, _ := r.Context().Value(plexPartKey{}).(bool)
	if plexPart {
		rep.contentType = plexPartMIME(s.name)
	}
	s.mu.Unlock()
	rep.body = func(w io.Writer, start, length int64) error {
		began := time.Now()
		err := m.streamClient(r.Context(), w, s, start, length)
		if m.opts.TracePlayback {
			m.log.Info("direct range", "session", s.id, "offset", start, "length", length,
				"took", time.Since(began), "err", err)
		}
		return err
	}
	serve := serveRepresentation
	if plexPart {
		serve = servePlexPart
	}
	if err := serve(w, r, rep); err != nil && r.Context().Err() == nil {
		m.log.Debug("direct play response ended early", "session", s.id, "err", err)
	}
}

// spliceOffset maps media time at in g to the packet-aligned offset in ng
// where an open response continues. A client parsing one packet size cannot
// continue into another, so a TS/BDAV mismatch ends the response instead.
func spliceOffset(s *session, g, ng *generation, at time.Duration) (int64, bool) {
	if !spliceable(s.container, s.name) {
		return 0, false
	}
	packet := tsPacketSize(s.container, s.name)
	info := ng.infoSnapshot()
	if tsPacketSize(info.Container, ng.src.Name()) != packet {
		return 0, false
	}
	return alignPacket(ng.timeMapSnapshot().OffsetAt(AlignTime(at, g.infoSnapshot().Duration, info.Duration)), packet), true
}

type directReadBudgetKey struct{}

// streamClient writes [vstart, vstart+length) of the session's virtual file.
func (m *Manager) streamClient(ctx context.Context, w io.Writer, s *session, vstart, length int64) error {
	if ctx.Value(directReadBudgetKey{}) == nil {
		ctx = context.WithValue(ctx, directReadBudgetKey{}, 25*time.Second)
	}
	g, compatible := s.directRangeGeneration()
	if !compatible {
		return errEndResponse
	}
	vend := vstart + length
	vpos := vstart
	srcpos := s.toSource(g, vpos)
	count := func(n int) {
		now := m.now()
		s.addBytes(n, now)
		if n > 0 {
			g.mu.Lock()
			g.retryEpoch++
			g.retryPending = false
			g.mu.Unlock()
			s.touch(now)
		}
	}
	for vpos < vend {
		n, rerr, werr := m.copyFrom(ctx, w, g, srcpos, vend-vpos, count, false)
		vpos += n
		srcpos += n
		if werr != nil {
			return werr
		}
		if rerr == nil {
			continue
		}
		var at time.Duration
		switch {
		case errors.Is(rerr, errRetired):
			// Another reader failed over while we were between chunks.
			ng := s.current()
			if ng == g || ng == nil {
				return rerr
			}
			if m.RequiresRemux(s.id) {
				return errEndResponse
			}
			at = g.timeMapSnapshot().TimeAt(srcpos)
			if vpos == vstart {
				g, srcpos = ng, s.toSource(ng, vpos)
				continue
			}
			pos, ok := spliceOffset(s, g, ng, at)
			if !ok {
				return errEndResponse
			}
			g, srcpos = ng, pos
			continue
		case errors.Is(rerr, errDirectReadDeadline) && ctx.Err() == nil:
			// The backend has not returned a chunk in the entire budget;
			// do not wait for its internal retries before replacing it.
			g.noteReadFailure(core.ErrSourceUnavailable)
			at = g.timeMapSnapshot().TimeAt(srcpos)
			res, err := m.failoverWith(ctx, s, g, at, failoverOpts{reason: "media source stalled", strictDirect: true, stallRecovery: true})
			if err == nil && res == g {
				// The stall grace continued this release on a read the
				// client may never repeat. This path does not return a
				// retryable error, so arm the same bounded recovery the
				// recoverable path uses: a client that stops requesting
				// still ends or restarts within two windows (issue #169).
				m.limitDirectRetry(s, g, at)
			}
			if err != nil && !errors.Is(err, errRequireRemux) && ctx.Err() == nil {
				m.emitEvent("error", s, err)
				m.Stop(s.id)
			}
			return errEndResponse
		case errors.Is(rerr, context.Canceled):
			// A newer seek abandoned this read, not the source.
			return rerr
		case errors.Is(rerr, io.EOF) && srcpos >= g.src.Size():
			// The source reached its real end before the virtual range did
			// (a spliced lower-bitrate release): not a read failure.
			return errShortSource
		case recoverableSourceFailure(rerr) && ctx.Err() == nil:
			boundedDirect := false
			if retry, ok := g.src.(core.DirectPlayRetrier); ok && errors.Is(rerr, core.ErrSourceUnavailable) && !permanentSourceFailure(rerr) {
				if retry.RetryDirectPlay() {
					// Keep coordinates for a brief interruption, but arm the
					// bounded recovery even if the client never retries.
					m.log.Warn("temporary direct-play read failure; retaining source for range retry", "session", s.id, "release", g.releaseID(), "offset", srcpos, "err", rerr)
					m.limitDirectRetry(s, g, g.timeMapSnapshot().TimeAt(srcpos))
					return rerr
				}
				// A provider pause cannot improve by retrying this source.
				boundedDirect = true
			}
			g.noteReadFailure(rerr)
			at = g.timeMapSnapshot().TimeAt(srcpos)
			ng, err := m.failoverWith(ctx, s, g, at, failoverOpts{reason: "source read failed during direct play", strictDirect: boundedDirect})
			if errors.Is(err, errRequireRemux) {
				return errEndResponse
			}
			if err != nil {
				if boundedDirect && ctx.Err() == nil {
					m.Stop(s.id)
					return errEndResponse
				}
				return err
			}
			if vpos == vstart {
				// Nothing of this range was written yet: map the whole range.
				g, srcpos = ng, s.toSource(ng, vpos)
				continue
			}
			pos, ok := spliceOffset(s, g, ng, at)
			if !ok {
				return errEndResponse
			}
			g, srcpos = ng, pos
			continue
		case errors.Is(rerr, io.EOF):
			return errShortSource
		default:
			return rerr
		}
	}
	return nil
}

// limitDirectRetry bounds an outage even if the client stops issuing ranges.
// Progress cancels the pending recovery, and each generation has one timer.
func (m *Manager) limitDirectRetry(s *session, g *generation, at time.Duration) {
	m.limitDirectRetryAfter(s, g, at, 25*time.Second)
}

func (m *Manager) limitDirectRetryAfter(s *session, g *generation, at, delay time.Duration) {
	g.mu.Lock()
	if g.retryPending {
		g.mu.Unlock()
		return
	}
	g.retryPending = true
	epoch := g.retryEpoch
	g.mu.Unlock()
	pending := func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.retryPending && g.retryEpoch == epoch && !g.retired
	}
	go func() {
		// One spent stall grace re-waits in this goroutine rather than
		// re-arming through limitDirectRetryAfter: capturing one epoch at
		// arm time keeps the epoch guarantee, so a read that progresses
		// between windows disarms the watchdog instead of letting a stale
		// timer re-arm over recovered progress and escalate later.
		for {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-g.ctx.Done():
				timer.Stop()
				return
			}
			if !pending() {
				return
			}
			g.noteReadFailure(core.ErrSourceUnavailable)
			ctx, cancel := context.WithTimeout(g.ctx, 25*time.Second)
			res, err := m.failoverWith(ctx, s, g, at, failoverOpts{reason: "media source unavailable", stillNeeded: pending, strictDirect: true, stallRecovery: true})
			continued := err == nil && res == g && pending()
			if err != nil && !errors.Is(err, errRequireRemux) && pending() {
				m.emitEvent("error", s, err)
				m.Stop(s.id)
			}
			cancel()
			if !continued {
				return
			}
		}
	}()
}

// directRangeCompatible keeps the original HTTP representation immutable even
// after SetDecision clears RequiresRemux for a newly started HLS session.
func (s *session) directRangeCompatible() bool {
	_, ok := s.directRangeGeneration()
	return ok
}

func (s *session) directRangeGeneration() (*generation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, s.cur != nil && !s.requiresRemux &&
		(spliceable(s.container, s.name) || (s.virtualGen != nil && s.cur.releaseID() == s.virtualGen.releaseID() && s.cur.src.Size() == s.virtual.Size))
}

type chunk struct {
	buf *[]byte
	n   int
	err error
}

// A cancelled request may leave a non-cooperative ReaderAt or Close running.
// Keep its slot until the producer actually exits, across all Managers using
// request retirement. Untracked callers preserve their existing admission.
const maxReadProducers = 128

var readProducers = make(chan struct{}, maxReadProducers)

// copyFrom copies up to length bytes of g's source starting at off into w,
// reading ahead of the writer in ChunkSize pieces. It returns the bytes
// written, the read error that stopped it (nil when length was copied) and
// any write error. A read error is reported only after every byte before
// the failure has been written.
func (m *Manager) copyFrom(ctx context.Context, w io.Writer, g *generation, off, length int64, onWrite func(int), background bool) (written int64, rerr, werr error) {
	if length <= 0 {
		return 0, nil, nil
	}
	if !background {
		m.installDamageHandler(g)
	}
	var retired func()
	tracker := core.ReadRetirementFrom(ctx)
	input, _ := ctx.Value(encoderInputKey{}).(*encoderInputScope)
	if tracker != nil || input != nil {
		if err := ctx.Err(); err != nil {
			return 0, err, nil
		}
		select {
		case readProducers <- struct{}{}:
		default:
			return 0, core.ErrNotAvailable, nil
		}
		var requestDone, inputDone func()
		if tracker != nil {
			var ok bool
			requestDone, ok = tracker.Begin()
			if !ok {
				<-readProducers
				return 0, errRetired, nil
			}
		}
		if input != nil {
			var ok bool
			inputDone, ok = input.begin(g)
			if !ok {
				<-readProducers
				if requestDone != nil {
					requestDone()
				}
				return 0, errRetired, nil
			}
		}
		retired = func() {
			<-readProducers
			if requestDone != nil {
				requestDone()
			}
			if inputDone != nil {
				inputDone()
			}
		}
	}
	if !g.acquire() {
		if retired != nil {
			retired()
		}
		return 0, errRetired, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g.sess.mu.Lock()
	analysis := background && g.sess.preparedOnly && g.sess.analysisSource
	active := g.sess.backgroundHold.active(m.now())
	g.sess.mu.Unlock()
	if analysis && !active {
		g.release()
		if retired != nil {
			retired()
		}
		return 0, core.ErrNotAvailable, nil
	}
	pool := &m.bufs
	depth := m.opts.ReadAhead
	if analysis {
		pool = &m.analysisBufs
		depth = min(max(depth, 1), 2)
	}
	ch := make(chan chunk, depth)
	go func() {
		defer func() {
			g.release()
			if retired != nil {
				retired()
			}
		}()
		defer close(ch)
		pos, rem := off, length
		for rem > 0 {
			if ctx.Err() != nil {
				return
			}
			bp := pool.Get().(*[]byte)
			want := int64(len(*bp))
			if rem < want {
				want = rem
			}
			var n int
			var err error
			readAt := time.Now()
			var readFinished func()
			if !background && g.sess.current() == g {
				readFinished = g.reads.begin(m.now())
			}
			if background {
				if cr, ok := g.src.(core.AnalysisReaderAt); ok && analysis {
					n, err = cr.ReadAtAnalysisContext(ctx, (*bp)[:want], pos)
				} else if cr, ok := g.src.(core.PrefetchReaderAt); ok {
					n, err = cr.ReadAtPrefetchContext(ctx, (*bp)[:want], pos)
				} else if cr, ok := g.src.(core.ContextReaderAt); ok {
					n, err = cr.ReadAtContext(ctx, (*bp)[:want], pos)
				} else {
					n, err = g.src.ReadAt((*bp)[:want], pos)
				}
			} else if cr, ok := g.src.(core.ConcurrentRangeReaderAt); ok {
				// Each HTTP response owns its producer, including overlapping
				// direct-play Cues and mid-file seeks. The backend bounds stalls.
				// Do not retire its remaining blocks with a per-chunk context.
				n, err = cr.ReadAtConcurrentContext(ctx, (*bp)[:want], pos)
			} else if cr, ok := g.src.(core.ContextReaderAt); ok {
				readCtx := ctx
				cancelRead := func() {}
				if retry, ok := g.src.(core.DirectPlayRetrier); ok && retry.RetryDirectPlay() {
					if budget, ok := ctx.Value(directReadBudgetKey{}).(time.Duration); ok && budget > 0 {
						readCtx, cancelRead = context.WithTimeout(ctx, budget)
					}
				}
				n, err = cr.ReadAtContext(readCtx, (*bp)[:want], pos)
				if err != nil && errors.Is(readCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
					err = errDirectReadDeadline
				}
				cancelRead()
			} else {
				n, err = g.src.ReadAt((*bp)[:want], pos)
			}
			if readFinished != nil {
				readFinished()
			}
			if n > 0 && g.sess.isRegistered() && g.sess.current() == g && !background {
				now := m.now()
				g.sess.noteFetch(n, time.Since(readAt), now)
				g.sess.noteReadPos(pos, n, now, m.opts.SpeedGrace)
			}
			if int64(n) == want && errors.Is(err, io.EOF) {
				err = nil
			}
			if background && err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				g.backgroundReadErrors.Add(1)
			}
			if err == nil && n == 0 {
				err = io.ErrNoProgress
			}
			if err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errDirectReadDeadline) && !recoverableSourceFailure(err) {
				// An active reader cannot produce a usable source after an
				// unclassified ReadAt failure either. Preserve the cause but
				// treat it as a transient source failure, never a permanent
				// release rejection or a reason to use estimated probe facts.
				err = fmt.Errorf("%w: %w", core.ErrSourceUnavailable, err)
			}
			select {
			case ch <- chunk{bp, n, err}:
			case <-ctx.Done():
				pool.Put(bp)
				return
			}
			if err != nil {
				return
			}
			pos += int64(n)
			rem -= int64(n)
		}
	}()
	for c := range ch {
		if c.n > 0 {
			k, err := w.Write((*c.buf)[:c.n])
			written += int64(k)
			onWrite(k)
			if err == nil && k < c.n {
				err = io.ErrShortWrite
			}
			if err != nil {
				pool.Put(c.buf)
				cancel()
				for c := range ch {
					pool.Put(c.buf)
				}
				return written, nil, err
			}
		}
		pool.Put(c.buf)
		if c.err != nil {
			return written, c.err, nil
		}
	}
	if err := ctx.Err(); err != nil && written < length {
		return written, err, nil
	}
	return written, nil, nil
}

// serveLoopback serves /src/{session}/{token}: one generation's source,
// byte for byte, to ffmpeg on 127.0.0.1. A read that hits missing articles
// ends the response and, for a live session, fails over in the background
// so the HLS session restarts on the next release.
func (m *Manager) serveLoopback(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	g := m.tokens[r.PathValue("session")+"/"+r.PathValue("token")]
	m.mu.Unlock()
	if g == nil {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	var inputErr error
	r, inputErr = m.encoderInputRequest(r, g)
	if inputErr != nil {
		http.Error(w, "unverified input", http.StatusForbidden)
		return
	}
	if cluster := r.URL.Query().Get("cluster"); cluster != "" {
		m.serveIndexedLoopback(w, r, g, cluster)
		return
	}
	s := g.sess
	s.mu.Lock()
	registered := s.registered
	s.mu.Unlock()
	// ffmpeg and background probes are producers, not viewers. Their reads
	// must not keep an abandoned playback alive.
	size := g.src.Size()
	sum := g.releaseID()
	rep := representation{
		size:        size,
		etag:        `"` + sum + "-" + g.token[:8] + `"`,
		contentType: contentTypeFor("", g.src.Name()),
	}
	background := r.URL.Query().Get("background") == "1"
	if background {
		g.backgroundRequests.Add(1)
		defer g.backgroundRequests.Add(-1)
	}
	count := func(n int) {
		if background {
			g.backgroundReadBytes.Add(int64(n))
		}
		if registered && !background {
			s.addBytes(n, m.now())
		}
	}
	rep.body = func(w io.Writer, start, length int64) error {
		began, first := time.Now(), true
		timed := func(n int) {
			if first {
				first = false
				// ffmpeg waits on this byte at every start and seek.
				if wait := time.Since(began); wait > 250*time.Millisecond && !background {
					m.log.Info("slow source start", "session", s.id, "release", g.releaseID(), "offset", start, "wait", wait)
				}
			}
			count(n)
		}
		n, rerr, werr := m.copyFrom(r.Context(), w, g, start, length, timed, background)
		if m.opts.TracePlayback {
			m.log.Info("loopback read", "session", s.id, "offset", start, "length", length, "written", n,
				"took", time.Since(began), "background", background, "err", errors.Join(rerr, werr))
		}
		if werr != nil {
			return werr
		}
		if rerr != nil && recoverableSourceFailure(rerr) && r.Context().Err() == nil && !background {
			g.noteReadFailure(rerr)
			m.log.Warn("source read failed", "session", s.id, "release", g.releaseID(), "offset", start+n, "err", rerr)
			if registered {
				m.backgroundFailover(s, g, g.timeMapSnapshot().TimeAt(start+n))
			}
		}
		return rerr
	}
	if err := serveRepresentation(w, r, rep); err != nil && r.Context().Err() == nil {
		m.log.Debug("loopback response ended early", "session", s.id, "err", err)
	}
}

// backgroundFailover fails a session over outside any request, for errors
// seen by ffmpeg on the loopback server.
func (m *Manager) backgroundFailover(s *session, g *generation, at time.Duration) {
	if s.isClosed() || !g.backgroundFailover.CompareAndSwap(false, true) {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		g.backgroundFailover.Store(false)
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer g.backgroundFailover.Store(false)
		ctx, cancel := context.WithTimeout(m.ctx, m.opts.FailoverTimeout)
		defer cancel()
		// Retiring the old generation is part of a successful input swap;
		// only stopping the session should cancel that swap's context.
		if s.stopCtx != nil {
			stop := context.AfterFunc(s.stopCtx, cancel)
			defer stop()
		}
		if _, err := m.failoverWith(ctx, s, g, at, failoverOpts{
			reason:         "source read failed while transcoding",
			preserveBuffer: true,
			followPlayer:   true,
		}); err != nil && ctx.Err() == nil && !errors.Is(err, errRequireRemux) {
			m.log.Error("background failover failed", "session", s.id, "err", err)
		}
	}()
}
