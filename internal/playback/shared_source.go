package playback

import (
	"context"
	"errors"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

// sharedHold is one Engine.Open shared by every session playing that payload.
// refs counts holders that have not called Close. The backend closes when the
// last holder leaves, so stopping one protocol does not stop another.
type sharedHold struct {
	src   core.MediaSource
	refs  int
	ready bool
	err   error
	wait  chan struct{}
}

type sharedSource struct {
	inner core.MediaSource
	hold  *sharedHold
	m     *Manager
	key   string
	once  sync.Once
	err   error
}

func (s *sharedSource) ReadAt(p []byte, off int64) (int, error) { return s.inner.ReadAt(p, off) }
func (s *sharedSource) Size() int64                             { return s.inner.Size() }
func (s *sharedSource) Name() string                            { return s.inner.Name() }
func (s *sharedSource) ReleaseID() string                       { return s.inner.ReleaseID() }
func (s *sharedSource) Close() error {
	s.once.Do(func() { s.err = s.m.releaseShared(s.key, s.hold) })
	return s.err
}

func sharedInner(src core.MediaSource) core.MediaSource {
	if shared, ok := src.(*sharedSource); ok && shared != nil && shared.inner != nil {
		return shared.inner
	}
	return src
}

func sharedClose(src core.MediaSource) func() error {
	if src == nil {
		return nil
	}
	return src.Close
}

func unwrapShared(src core.MediaSource) (core.MediaSource, func() error) {
	return sharedInner(src), sharedClose(src)
}

func (m *Manager) openShared(ctx context.Context, res *core.Resolution) (core.MediaSource, error) {
	if m == nil || m.deps.Engine == nil || res == nil {
		return nil, errors.New("playback: backend returned no source")
	}
	if res.Release.ID == "" || len(res.NZB) == 0 {
		return m.deps.Engine.Open(ctx, res.Release.ID, res.NZB)
	}
	key := res.Release.ID + "\x00" + nzbHash(res.NZB)
	for {
		m.sharedMu.Lock()
		if m.shared == nil {
			m.shared = map[string]*sharedHold{}
		}
		h := m.shared[key]
		if h != nil && h.ready && h.err == nil && h.src != nil {
			h.refs++
			m.sharedMu.Unlock()
			return &sharedSource{inner: h.src, hold: h, m: m, key: key}, nil
		}
		if h != nil && !h.ready {
			wait := h.wait
			m.sharedMu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		h = &sharedHold{wait: make(chan struct{}), refs: 1}
		m.shared[key] = h
		m.sharedMu.Unlock()

		src, err := m.deps.Engine.Open(ctx, res.Release.ID, res.NZB)
		m.sharedMu.Lock()
		h.src, h.err, h.ready = src, err, true
		if err != nil || src == nil {
			if m.shared[key] == h {
				delete(m.shared, key)
			}
			close(h.wait)
			m.sharedMu.Unlock()
			if src != nil {
				_ = src.Close()
			}
			if err == nil {
				err = errors.New("playback: backend returned no source")
			}
			return nil, err
		}
		close(h.wait)
		m.sharedMu.Unlock()
		return &sharedSource{inner: src, hold: h, m: m, key: key}, nil
	}
}

func (m *Manager) releaseShared(key string, h *sharedHold) error {
	if h == nil {
		return nil
	}
	m.sharedMu.Lock()
	h.refs--
	if h.refs > 0 {
		m.sharedMu.Unlock()
		return nil
	}
	src := h.src
	if m.shared != nil && m.shared[key] == h {
		delete(m.shared, key)
	}
	m.sharedMu.Unlock()
	if src == nil {
		return nil
	}
	return src.Close()
}
