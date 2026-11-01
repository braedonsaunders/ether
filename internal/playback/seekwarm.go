package playback

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Seek warming. When an encoder run starts at a position, the first output
// segment needs every byte from the keyframe at or before the start to the
// keyframe after the segment ends: often a whole GOP or two. ffmpeg asks for
// those bytes only after it has spawned, re-read the header and located the
// cluster, and then reads them through one sequential stream. Warming fetches
// that span the moment the run is planned, from the container's keyframe
// index, split across parallel reads, so the encoder finds it in the cache.

const (
	warmMinBytes   = 2 << 20
	warmMaxBytes   = 48 << 20
	warmPartBytes  = 6 << 20
	warmParallel   = 4
	warmUnindexed  = 12 * time.Second // GOP slack when no keyframe index is known yet
	warmReadBudget = 20 * time.Second
)

type warmState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

// WarmInput prefetches the bytes an encoder run over [from, to) of the
// generation behind inputURL will need first. It returns at once; a newer
// warm for the same session cancels an older one.
func (m *Manager) WarmInput(inputURL string, from, to time.Duration) {
	g := m.generationFor(inputURL)
	if g == nil || to <= from || !g.acquire() {
		return
	}
	lo, hi := g.warmSpan(from, to)
	if hi-lo < 1 {
		g.release()
		return
	}
	s := g.sess
	ctx, cancel := context.WithTimeout(g.ctx, warmReadBudget)
	s.warm.mu.Lock()
	if s.warm.cancel != nil {
		s.warm.cancel()
	}
	s.warm.cancel = cancel
	s.warm.mu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		g.release()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer g.release()
		defer cancel()
		started := time.Now()
		var wg sync.WaitGroup
		sem := make(chan struct{}, warmParallel)
		var mu sync.Mutex
		var warmed int64
	parts:
		for off := lo; off < hi; off += warmPartBytes {
			n := min(warmPartBytes, hi-off)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break parts
			}
			wg.Add(1)
			go func(off, n int64) {
				defer wg.Done()
				defer func() { <-sem }()
				buf := make([]byte, n)
				got, _ := readMetadata(ctx, g, buf, off)
				mu.Lock()
				warmed += int64(got)
				mu.Unlock()
			}(off, n)
		}
		wg.Wait()
		if m.opts.TracePlayback {
			m.log.Info("seek warm", "session", s.id, "from", from, "offset", lo, "bytes", hi-lo, "warmed", warmed, "took", time.Since(started), "cancelled", ctx.Err() != nil)
		}
	}()
}

// warmSpan is the byte range covering [from, to) snapped outward to
// keyframes when the index is known, padded by a GOP's worth otherwise.
func (g *generation) warmSpan(from, to time.Duration) (int64, int64) {
	tm := g.timeMapSnapshot()
	if !tm.Valid() {
		return 0, 0
	}
	var lo, hi int64
	if len(tm.Points) > 0 {
		lo = tm.KeyframeOffsetAt(from)
		hi = tm.Size
		for _, p := range tm.Points {
			if p.Time > to {
				hi = p.Offset
				break
			}
		}
	} else {
		lo = tm.OffsetAt(max(0, from-warmUnindexed))
		hi = tm.OffsetAt(to + warmUnindexed/2)
	}
	if hi-lo < warmMinBytes {
		hi = lo + warmMinBytes
	}
	hi = min(hi, lo+warmMaxBytes, tm.Size)
	return max(0, lo), hi
}

// generationFor finds the live generation a loopback input URL names.
func (m *Manager) generationFor(inputURL string) *generation {
	u, err := url.Parse(inputURL)
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "src" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens[parts[1]+"/"+parts[2]]
}

// readMetadata reads without counting as the viewer's sequential position,
// so warming never disturbs read-ahead or seek detection.
func readMetadata(ctx context.Context, g *generation, p []byte, off int64) (int, error) {
	// Prefetch priority so a live encoder Range can steal the connection
	// instead of waiting behind a 48 MiB warm of the GOP at the seek.
	if pr, ok := g.src.(core.PrefetchReaderAt); ok {
		return pr.ReadAtPrefetchContext(ctx, p, off)
	}
	if mr, ok := g.src.(core.MetadataReaderAt); ok {
		return mr.ReadMetadataAtContext(ctx, p, off)
	}
	if cr, ok := g.src.(core.ContextReaderAt); ok {
		return cr.ReadAtContext(ctx, p, off)
	}
	return g.src.ReadAt(p, off)
}
