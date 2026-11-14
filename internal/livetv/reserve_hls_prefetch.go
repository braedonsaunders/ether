package livetv

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"
)

// A Steady decoder reads its HLS reserve one segment at a time, so without
// help a segment the provider is slow to answer stalls it however deep the
// reserve is. For a Steady grant the relay therefore fetches every segment
// the provider lists ahead of the reader, once the reader is reading that
// playlist: a slow segment starts as soon as it is listed, a reserve's
// length before the decoder needs it, and the decoder is served from
// memory. Fetches run in playlist order and stop at a memory budget, past
// which the reader fetches for itself as before.
//
// Concurrency follows the provider account. Each channel's own connection
// is one fetch at a time, exactly what the decoder did itself. A second
// fetch alongside it is an extra connection as far as a panel that counts
// HLS requests is concerned, so it runs only when the account's limiter
// (the one VOD and metadata reads use, which counts every live channel and
// broadcast) grants a slot. A one-connection account, or a full one, is
// strictly sequential.

const (
	hlsPrefetchBudget  = 96 << 20 // bytes held per grant
	hlsSegmentLimit    = 32 << 20 // largest segment kept in memory
	hlsPrefetchTimeout = 30 * time.Second
	// hlsSkipAfter is how long the decoder waits on a segment still in
	// flight before it is told to skip it, when a later one is ready.
	hlsSkipAfter = 1500 * time.Millisecond
)

// hlsFetch is one prefetched segment; fields are set before done closes.
type hlsFetch struct {
	seq    int64
	url    string
	done   chan struct{}
	status int
	ctype  string
	body   []byte
	err    error
}

// prefetchLocked starts fetches for the listed segments the reader has not
// reached, in order, within the budget. Only for a Steady grant, and only
// for a playlist the reader has fetched a segment from (a probe also opens
// variants it then drops).
func (h *hlsInput) prefetchLocked(l *hlsList) {
	if h.g.reserve <= 0 || l.served < 0 || h.g.ctx == nil || h.g.ctx.Err() != nil {
		return
	}
	if h.fetches == nil {
		h.fetches = map[string]*hlsFetch{}
	}
	for _, sg := range l.last {
		if sg.seq <= l.served || h.fetches[sg.url] != nil {
			continue
		}
		if h.held >= hlsPrefetchBudget {
			break
		}
		f := &hlsFetch{seq: sg.seq, url: sg.url, done: make(chan struct{})}
		h.fetches[sg.url] = f
		h.queue = append(h.queue, f)
	}
	// Fetches run strictly in playlist order: the channel's own fetcher,
	// plus one helper when the account has room for another connection.
	// Each takes the earliest waiting segment, so a stuck segment holds up
	// one fetcher, never the order of the rest.
	if len(h.queue) > 0 && !h.fetching {
		h.fetching = true
		go h.work(func() {})
	}
	if len(h.queue) > 1 && !h.helping {
		h.helping = true
		go h.help()
	}
}

// next pops the earliest queued fetch; nil when there is none.
func (h *hlsInput) next() *hlsFetch {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) == 0 || h.g.ctx.Err() != nil {
		return nil
	}
	f := h.queue[0]
	h.queue = h.queue[1:]
	return f
}

// work fetches queued segments in order until the queue is empty.
func (h *hlsInput) work(release func()) {
	defer release()
	for {
		f := h.next()
		if f == nil {
			h.mu.Lock()
			if len(h.queue) > 0 {
				h.mu.Unlock()
				continue
			}
			h.fetching = false
			h.mu.Unlock()
			return
		}
		h.fetchOne(f)
	}
}

// help is the second fetcher, running only on a slot the account's
// limiter grants (see extraSlot).
func (h *hlsInput) help() {
	release := h.extraSlot(h.g.service)
	if release == nil {
		h.mu.Lock()
		h.helping = false
		h.mu.Unlock()
		return
	}
	defer release()
	for {
		f := h.next()
		if f == nil {
			h.mu.Lock()
			h.helping = false
			h.mu.Unlock()
			return
		}
		h.fetchOne(f)
	}
}

func (h *hlsInput) fetchOne(f *hlsFetch) {
	defer close(f.done)
	s := h.g.service
	if s == nil {
		f.err = errRelayUnsupported
		return
	}
	ctx, cancel := context.WithTimeout(h.g.ctx, hlsPrefetchTimeout)
	defer cancel()
	res, err := s.relayFetchAs(ctx, cancel, h.g, f.url, &http.Request{Header: http.Header{}}, true)
	if err != nil {
		f.err = err
		return
	}
	defer res.Body.Close()
	f.status, f.ctype = res.StatusCode, res.Header.Get("Content-Type")
	if res.StatusCode != http.StatusOK {
		return
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, hlsSegmentLimit+1))
	if err != nil || len(b) > hlsSegmentLimit {
		f.err, f.status = errRelayUnsupported, 0
		return
	}
	f.body = b
	h.mu.Lock()
	h.held += len(b)
	h.mu.Unlock()
}

// extraSlot grants a fetch beyond the channel's own connection through the
// account limiter; nil when the account has no room.
func (h *hlsInput) extraSlot(s *Service) func() {
	if s == nil || h.g.login != nil {
		return nil
	}
	if h.g.source != "" {
		if limit := s.sourceConnectionLimit(h.g.source); limit > 0 {
			release, err := s.reserveVODProvider(h.g.ctx, h.g.source, limit, false)
			if err != nil {
				return nil
			}
			return release
		}
	}
	return func() {}
}

// prefetched returns the fetch for a segment, started or finished.
func (h *hlsInput) prefetched(raw string) *hlsFetch {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fetches[raw]
}

// laterReady reports a completed fetch of a segment after seq.
func (h *hlsInput) laterReady(seq int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.fetches {
		if f.seq > seq {
			select {
			case <-f.done:
				if f.err == nil && f.status == http.StatusOK {
					return true
				}
			default:
			}
		}
	}
	return false
}

// evictLocked drops fetches the reader is past (keeping the one before its
// position, which a restarted decoder reads first).
func (h *hlsInput) evictLocked(served int64) {
	for raw, f := range h.fetches {
		if f.seq < served-1 {
			select {
			case <-f.done:
				h.held -= len(f.body)
				delete(h.fetches, raw)
			default:
			}
		}
	}
}

// servePrefetched answers a reader's segment request from a prefetch,
// waiting for one still in flight. It reports false when the segment was
// not prefetched.
func (s *Service) servePrefetched(w http.ResponseWriter, r *http.Request, g *inputGrant, raw string) bool {
	h := g.hlsIfAny()
	if h == nil {
		return false
	}
	f := h.prefetched(raw)
	if f == nil {
		return false
	}
	skip := time.NewTicker(hlsSkipAfter)
	defer skip.Stop()
wait:
	for {
		select {
		case <-f.done:
			break wait
		case <-r.Context().Done():
			return true
		case <-g.ctx.Done():
			http.Error(w, "input revoked", http.StatusGone)
			return true
		case <-skip.C:
			// Still in flight while a later segment is ready: skip this
			// one (ffmpeg's demuxer moves on at once) rather than starve
			// the decoder with its reserve sitting in memory.
			if h.laterReady(f.seq) {
				h.servedSegment(raw)
				h.segmentFailed()
				http.Error(w, "segment late", http.StatusBadGateway)
				return true
			}
		}
	}
	h.servedSegment(raw)
	if f.err != nil || f.status != http.StatusOK {
		h.segmentFailed()
		code := f.status
		if code < 400 || code > 599 {
			code = http.StatusBadGateway
		}
		http.Error(w, "upstream unavailable", code)
		return true
	}
	if mt, params, err := mime.ParseMediaType(f.ctype); err == nil {
		w.Header().Set("Content-Type", mime.FormatMediaType(mt, params))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(f.body)
	return true
}
