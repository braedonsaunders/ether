package transcode

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// One joined worker per manager serializes setup. Config changes cancel the
// current attempt and replace queued work; no install goroutine outlives Close.
// Fields except ctx/wake are protected by Manager.mu.
type rtxSetupWorker struct {
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	attempt context.CancelFunc
	epoch   uint64
	resolve func(context.Context, config.RTXVideo, func(string, string)) (string, core.VideoEnhancementSupport)
}

func (w *rtxSetupWorker) changedLocked() {
	w.epoch++
	if w.attempt != nil {
		w.attempt()
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) startRTXSetup(goos string) {
	if m.opts.runtimeDir == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &rtxSetupWorker{done: make(chan struct{}), ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	resolver := rtxSetupResolver{goos: goos, goarch: runtime.GOARCH,
		probe: func(ctx context.Context, path string) (string, core.VideoEnhancementSupport) {
			return probeRTX(ctx, m.runner, goos, path)
		},
		gpu:     func(ctx context.Context) error { return checkRTXGPU(ctx, m.runner) },
		install: newRTXInstaller(m.opts.runtimeDir, m.runner).ensure}
	w.resolve = resolver.resolve
	m.rtxSetup = w
	m.wg.Add(1)
	go m.runRTXSetup(w)
	m.mu.Lock()
	w.changedLocked()
	m.mu.Unlock()
}

func (m *Manager) runRTXSetup(w *rtxSetupWorker) {
	defer m.wg.Done()
	if w.done != nil {
		defer close(w.done)
	}
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.wake:
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		cfg, epoch := m.cfg.RTXVideo, w.epoch
		ctx, cancel := context.WithTimeout(w.ctx, 15*time.Minute)
		w.attempt = cancel
		m.mu.Unlock()
		publish := func(path string, support core.VideoEnhancementSupport) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.closed || epoch != w.epoch {
				return
			}
			m.rtx.mu.Lock()
			m.rtx.path, m.rtx.support, m.rtx.configuredPath = path, support, cfg.ExecutablePath
			m.rtx.mu.Unlock()
		}
		progress := func(state, reason string) {
			publish("", core.VideoEnhancementSupport{Backend: rtxBackend, SetupState: state, Reason: reason})
		}
		progress("checking", "Checking RTX Video configuration")
		path, support := w.resolve(ctx, cfg, progress)
		cancel()
		publish(path, support)
		m.mu.Lock()
		if epoch == w.epoch {
			w.attempt = nil
		}
		m.mu.Unlock()
	}
}

type rtxSetupResolver struct {
	goos, goarch string
	probe        func(context.Context, string) (string, core.VideoEnhancementSupport)
	gpu          func(context.Context) error
	install      func(context.Context) (string, error)
}

func (r rtxSetupResolver) resolve(ctx context.Context, cfg config.RTXVideo, progress func(string, string)) (string, core.VideoEnhancementSupport) {
	unavailable := func(reason string) (string, core.VideoEnhancementSupport) {
		return "", core.VideoEnhancementSupport{Backend: rtxBackend, SetupState: "unavailable", Reason: reason}
	}
	normalized, invalid := normalizedRTX(cfg)
	if invalid != "" {
		return unavailable(invalid)
	}
	if normalized.Mode == "off" {
		return "", core.VideoEnhancementSupport{Backend: rtxBackend, SetupState: "off", Reason: "Disabled by owner; enabling RTX Video automatically installs its runtime on compatible Windows hardware"}
	}
	if r.goos != "windows" || r.goarch != "amd64" {
		return unavailable("RTX Video requires native Windows x64 and an NVIDIA RTX GPU")
	}
	// Explicit owner paths are never replaced, repaired or downloaded over.
	if cfg.ExecutablePath != "" {
		return r.probe(ctx, cfg.ExecutablePath)
	}
	// Honor an already installed compatible runtime before provisioning.
	if path, support := r.probe(ctx, ""); support.Available {
		return path, support
	}
	progress("checking", "Checking NVIDIA RTX hardware before runtime installation")
	if err := r.gpu(ctx); err != nil {
		return unavailable(err.Error())
	}
	progress("installing", "Installing verified NVEncC 9.38 runtime; ordinary transcoding remains available")
	path, err := r.install(ctx)
	if err != nil {
		return unavailable(fmt.Sprintf("RTX Video setup failed: %v; ordinary transcoding remains available. Toggle RTX Video off/on to retry", err))
	}
	progress("checking", "Validating installed NVEncC/NGX runtime")
	return r.probe(ctx, path)
}
