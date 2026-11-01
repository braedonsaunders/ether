package playback

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type heldIndexSource struct {
	core.MediaSource
	started chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (s *heldIndexSource) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.release:
	}
	return s.MediaSource.ReadAt(p, off)
}

type heldIndexEngine struct {
	core.Engine
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (e *heldIndexEngine) Open(ctx context.Context, id string, p []byte) (core.MediaSource, error) {
	src, err := e.Engine.Open(ctx, id, p)
	if err != nil {
		return nil, err
	}
	return &heldIndexSource{MediaSource: src, started: e.started, release: e.release, once: &e.once}, nil
}
func TestWarmReadyAndPlayDoNotWaitForSeekIndex(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	engine := &heldIndexEngine{Engine: h.engine, started: make(chan struct{}), release: make(chan struct{})}
	h.m.deps.Engine = engine
	// Closing the gate before harness shutdown also covers a non-contextual
	// source implementation finishing a read.
	defer close(engine.release)
	caps := defaultCaps("fast-start")
	if !h.m.PreResolve(context.Background(), owner, movieKey, caps) {
		t.Fatal("warm skipped")
	}
	select {
	case <-engine.started:
	case <-time.After(time.Second):
		t.Fatal("index not started")
	}
	h.m.mu.Lock()
	entry := h.m.warm[identity{userID: owner.ID, clientID: caps.ClientID, key: movieKey}]
	h.m.mu.Unlock()
	select {
	case <-entry.done:
	case <-time.After(time.Second):
		t.Fatal("seek index blocked warm readiness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ps, err := h.m.Prepare(ctx, owner, movieKey, caps)
	if err != nil {
		t.Fatalf("Play waited for index: %v", err)
	}
	if ps.Info.Estimated {
		t.Fatal("warm lost its real probe")
	}
	if resolves, probes := counts(h); resolves != 1 || probes != 1 {
		t.Fatalf("warm missed: resolves=%d probes=%d", resolves, probes)
	}
}
