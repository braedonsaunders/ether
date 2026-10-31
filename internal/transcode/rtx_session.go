package transcode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// prepareRTXRunLocked runs after the old process has been reaped. The base
// retains its native plan while a lower ordinary rung is active, so a later
// base request can retry only through the usual runtime/permit/seek checks.
func (s *Session) prepareRTXRunLocked(rp *runPlan, seg int) {
	if s.decision.Enhancement.Backend != rtxBackend {
		return
	}
	if !enhancementSelected(rp.decision) {
		if s.wantTrack != 0 && len(s.rungs) > 0 && enhancementSelected(s.rungs[0].decision) {
			s.decision.Enhancement = core.VideoEnhancement{Backend: rtxBackend, State: "fallback", Reason: "ordinary ABR rendition active; enhancement reserved for base"}
		}
		return
	}
	if gp := s.goodput.Kbps(); gp > 0 && gp*9/10 < decisionTotalKbps(s.info, rp.decision) {
		s.fallbackRTXLocked("measured bandwidth is below enhanced base; ordinary transcoder required", seg)
		rp.decision = s.trackDecisionLocked(s.wantTrack)
	}
}

// startRTXRun either returns a native backend process or records an
// explicit fallback and lets the caller start ordinary FFmpeg. The permit is
// released only after the OS process is reaped, including start failures.
func (m *Manager) startRTXRun(spec runSpec, cfg config.RTXVideo, dir string, stdout, stderr *lineWriter) (Process, []string, error) {
	if _, reason := normalizedRTX(cfg); reason != "" {
		return nil, nil, fmt.Errorf("RTX Video: %s", reason)
	}
	if cfg.Mode == "off" || cfg.Mode == "" {
		return nil, nil, fmt.Errorf("RTX Video disabled by owner")
	}
	if v := spec.Info.Video(); v == nil || v.Height > cfg.SourceMaxHeight || spec.Decision.Enhancement.Height > cfg.TargetMaxHeight {
		return nil, nil, fmt.Errorf("RTX Video source or target exceeds owner limits")
	}
	args, keys, err := buildRTXArgs(spec)
	if err != nil {
		return nil, nil, err
	}
	b := &m.rtx
	b.mu.Lock()
	if !b.support.Available || b.path == "" {
		b.mu.Unlock()
		return nil, nil, fmt.Errorf("RTX Video backend unavailable")
	}
	if b.active >= cfg.MaxConcurrent {
		b.mu.Unlock()
		return nil, nil, fmt.Errorf("RTX Video enhancement concurrency cap reached")
	}
	b.active++
	path := b.path
	b.mu.Unlock()
	release := func() { b.mu.Lock(); b.active--; b.mu.Unlock() }
	if err := os.WriteFile(filepath.Join(spec.Dir, "rtx-keyframes.txt"), keys, 0o600); err != nil {
		release()
		return nil, nil, fmt.Errorf("RTX Video keyframe plan: %w", err)
	}
	start := m.runner.Start
	if owner, ok := m.runner.(RTXRunner); ok {
		start = owner.StartRTX
	}
	p, err := start(Command{Path: path, Args: args, Dir: dir, Stdout: stdout, Stderr: stderr})
	if p == nil {
		release()
		return nil, nil, fmt.Errorf("RTX Video executable failed to start")
	}
	held := &rtxPermitProcess{Process: p, done: make(chan struct{}), release: release}
	go func() { <-p.Done(); held.once.Do(release); close(held.done) }()
	if err != nil {
		return held, args, fmt.Errorf("RTX Video executable failed to start")
	}
	return held, args, nil
}

type rtxPermitProcess struct {
	Process
	done    chan struct{}
	once    sync.Once
	release func()
}

func (p *rtxPermitProcess) Done() <-chan struct{} { return p.done }
func (p *rtxPermitProcess) Stop()                 { p.Process.Stop(); <-p.done }

// fallbackRTXLocked removes the enhancement plan from every rendition and
// refreshes manifest dimensions. It never labels an ordinary scaler RTX VSR.
func (s *Session) fallbackRTXLocked(reason string, seg int) {
	old := s.decision.Enhancement
	if old.Backend != rtxBackend {
		return
	}
	s.decision.Enhancement = core.VideoEnhancement{Backend: rtxBackend, State: "fallback", Reason: reason}
	s.decision.Reason += "; RTX Video fallback: " + reason
	v := s.info.Video()
	if v != nil {
		s.decision.MaxHeight = minPositive(s.decision.MaxHeight, v.Height)
	}
	oldWidth, oldHeight := 0, 0
	if len(s.rungs) > 0 {
		oldWidth, oldHeight = s.rungs[0].width, s.rungs[0].height
	}
	for i := range s.rungs {
		d := s.rungs[i].decision
		if i == 0 {
			d.Enhancement = s.decision.Enhancement
		} else {
			d.Enhancement = core.VideoEnhancement{}
		}
		if v != nil {
			d.MaxHeight = minPositive(d.MaxHeight, v.Height)
			s.rungs[i].width, s.rungs[i].height = decisionOutputSize(v, d)
		}
		s.rungs[i].decision = d
		s.rungs[i].codecs = rungCodecs(s.info, d, s.rungs[i].height)
	}
	if len(s.rungs) > 0 && (oldWidth != s.rungs[0].width || oldHeight != s.rungs[0].height) {
		// A new ordinary init cannot describe cached enhanced video. Keep
		// the timeline and lower caches, but regenerate base on any revisit.
		if len(s.done) > 0 {
			for n := range s.done[0] {
				_ = os.Remove(s.trackSegPath(0, n))
				delete(s.done[0], n)
			}
		}
		s.initFile = ""
		if len(s.initFiles) > 0 {
			s.initFiles[0] = ""
		}
		if s.dash != nil && len(s.dash.inits) > 0 {
			s.dash.inits[0] = ""
			if len(s.dash.codecs) > 0 {
				s.dash.codecs[0] = ""
			}
		}
	}
	if seg > 0 {
		if s.rtxDiscontinuities == nil {
			s.rtxDiscontinuities = map[int]bool{}
		}
		s.rtxDiscontinuities[seg] = true
	}
	s.playlist = nil
	if s.dash != nil {
		// Transport shape/track selection is unchanged; only video dimensions
		// change. collectDASH publishes this run's new initialization segment.
		for i := range s.dash.reps {
			rep := &s.dash.reps[i]
			if rep.Kind != "video" {
				continue
			}
			for _, next := range dashReps(s.info, s.trackDecisionLocked(rep.ID)) {
				if next.Kind == "video" {
					rep.Width, rep.Height, rep.Codecs, rep.Bandwidth = next.Width, next.Height, next.Codecs, next.Bandwidth
					break
				}
			}
		}
		s.dash.mpd = nil
	}
	s.log.Warn("RTX Video fallback", "reason", reason)
}

func redactRTXLine(line, input string, headers map[string]string) string {
	if input != "" {
		line = strings.ReplaceAll(line, input, "[source]")
	}
	for _, value := range headers {
		if value != "" {
			line = strings.ReplaceAll(line, value, "[redacted]")
		}
	}
	return line
}

// Preserve optional runner diagnostics through the concurrency wrapper.
func (p *rtxPermitProcess) PID() int {
	if x, ok := p.Process.(core.ProcessIdentity); ok {
		return x.PID()
	}
	return 0
}
func (p *rtxPermitProcess) CPUTime() (time.Duration, time.Duration) {
	if x, ok := p.Process.(core.ProcessCPUTime); ok {
		return x.CPUTime()
	}
	return 0, 0
}
