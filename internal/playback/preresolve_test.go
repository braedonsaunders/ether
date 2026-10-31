package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPreResolveReusedWithoutLiveSessionOrHistory(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	// Keep the first warm in flight while the second asks: the per-user
	// budget counts warms that are still running.
	h.resolver.mu.Lock()
	h.resolver.delay = 200 * time.Millisecond
	h.resolver.mu.Unlock()
	caps := defaultCaps("client")
	if !h.m.PreResolve(context.Background(), owner, movieKey, caps) {
		t.Fatal("first warm skipped")
	}
	if h.m.PreResolve(context.Background(), owner, movie2, caps) {
		t.Fatal("per-user budget ignored")
	}
	h.m.mu.Lock()
	e := h.m.warm[identity{userID: owner.ID, clientID: caps.ClientID, key: movieKey}]
	h.m.mu.Unlock()
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("warm did not complete")
	}
	if len(h.m.Sessions()) != 0 {
		t.Fatal("speculation created live session")
	}
	// Metadata omits decision-only codec and bandwidth hints. The same profile
	// must reuse its release and probe even several minutes later.
	h.clock.Advance(5 * time.Minute)
	caps.MaxBitrateKbps = 20000
	caps.VideoCodecs = []string{"h264"}
	ps, err := h.m.Prepare(context.Background(), owner, movieKey, caps)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Resolution.Release.ID != "r1" {
		t.Fatalf("wrong warm release %+v", ps)
	}
	h.resolver.mu.Lock()
	calls := h.resolver.calls
	h.resolver.mu.Unlock()
	h.tr.mu.Lock()
	probes := h.tr.probes
	h.tr.mu.Unlock()
	if probes != 1 {
		t.Fatalf("warm probe was repeated: %d", probes)
	}
	if calls != 1 {
		t.Fatalf("resolved again: %d calls", calls)
	}
}

func TestSurfaceVersionUsesMatchingWarmProbe(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	caps := defaultCaps("client")
	caps.Protocol = "jellyfin"
	if !h.m.PreResolve(context.Background(), owner, movieKey, caps) {
		t.Fatal("warm skipped")
	}
	h.m.mu.Lock()
	e := h.m.warm[identity{userID: owner.ID, clientID: caps.ClientID, key: movieKey, surface: "jellyfin"}]
	h.m.mu.Unlock()
	if e == nil {
		t.Fatal("surface warm missing")
	}
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("warm did not complete")
	}
	ps, err := h.m.PrepareSurface(context.Background(), owner, movieKey, caps, "r1", true)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Resolution == nil || ps.Resolution.Release.ID != "r1" {
		t.Fatal("exact source changed")
	}
	h.resolver.mu.Lock()
	calls := h.resolver.calls
	h.resolver.mu.Unlock()
	h.tr.mu.Lock()
	probes := h.tr.probes
	h.tr.mu.Unlock()
	if calls != 1 || probes != 1 {
		t.Fatalf("matching warm repeated resolve/probe: %d/%d", calls, probes)
	}
}

func TestCachePolicyCanDisableSpeculation(t *testing.T) {
	for _, mode := range []string{"off", "detail", "all"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
			cfg := h.m.config()
			switch mode {
			case "off":
				cfg.Cache.Mode = "off"
			case "detail":
				cfg.Cache.DetailWarmup = "off"
			case "all":
				cfg.Cache.DisableSpeculation = true
			}
			h.m.SetConfig(cfg)
			if h.m.PreResolve(context.Background(), owner, movieKey, defaultCaps("client")) {
				t.Fatal("disabled warm was admitted")
			}
			h.resolver.mu.Lock()
			defer h.resolver.mu.Unlock()
			if h.resolver.calls != 0 {
				t.Fatal("disabled warming reached resolver")
			}
		})
	}
}

type openingPrefetchFailure struct {
	core.Engine
	err      error
	finished chan context.Context
}

func (e *openingPrefetchFailure) Prefetch(ctx context.Context, _ string, _ []byte, _, _ int64) error {
	e.finished <- ctx
	return e.err
}

func TestPrewarmRejectsMissingOpeningButNotTransientFailure(t *testing.T) {
	for _, permanent := range []bool{true, false} {
		name := "transient"
		failure := context.DeadlineExceeded
		if permanent {
			name = "missing"
			failure = core.ErrArticleMissing
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
			engine := &openingPrefetchFailure{Engine: h.engine, err: failure, finished: make(chan context.Context, 1)}
			h.m.deps.Engine = engine
			if !h.m.PreResolve(context.Background(), owner, movieKey, defaultCaps("client")) {
				t.Fatal("warm skipped")
			}
			select {
			case ctx := <-engine.finished:
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
					t.Fatal("warm did not finish")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("prefetch not reached")
			}
			// Warm cancellation happens after rejection processing.
			h.resolver.mu.Lock()
			_, bad := h.resolver.bad["r1"]
			h.resolver.mu.Unlock()
			h.m.mu.Lock()
			_, warm := h.m.warm[identity{userID: owner.ID, clientID: "client", key: movieKey}]
			h.m.mu.Unlock()
			if bad != permanent || warm == permanent {
				t.Fatalf("bad=%v warm=%v permanent=%v", bad, warm, permanent)
			}
		})
	}
}

func TestAwaitWarmProbeWaitsForTheInFlightProbe(t *testing.T) {
	probed := core.MediaInfo{
		Container: "mp4", Duration: 20 * time.Second, BitrateKbps: 17,
		Streams: []core.StreamInfo{
			{Type: "video", Codec: "h264", Width: 320, Height: 180},
			{Type: "audio", Codec: "aac", Language: "fr"},
			{Type: "audio", Codec: "aac", Language: "en"},
			{Type: "subtitle", Codec: "mov_text", Language: "en"},
		},
	}
	user := &core.User{ID: 9}
	caps := core.ClientCaps{ClientID: "adult"}
	key := core.MovieKey(10004)
	done := make(chan struct{})
	m := &Manager{
		now:  time.Now,
		warm: map[identity]*warmEntry{},
	}
	m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}] = &warmEntry{
		done:    done,
		expires: time.Now().Add(time.Minute),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := make(chan core.MediaInfo, 1)
	go func() {
		info, ok := m.AwaitWarmProbe(ctx, user, caps, key)
		if !ok {
			got <- core.MediaInfo{Estimated: true}
			return
		}
		got <- info
	}()
	select {
	case info := <-got:
		t.Fatalf("returned before the probe finished: %+v", info)
	case <-time.After(30 * time.Millisecond):
	}
	m.mu.Lock()
	m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}].probe = &probeCache{info: probed}
	m.mu.Unlock()
	close(done)
	select {
	case info := <-got:
		if info.Estimated || info.Container != "mp4" || info.Duration != 20*time.Second || len(info.Streams) != 4 {
			t.Fatalf("probe = %+v", info)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the probe")
	}
	if _, ok := m.WarmProbeInfo(user, caps, key); !ok {
		t.Fatal("completed probe was not readable")
	}
}
