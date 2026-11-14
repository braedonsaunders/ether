package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ActionJobTotal    = 12 * time.Hour
	ActionJobAttempts = 1000000
	actionJobMinDelay = 100 * time.Millisecond
)

// ActionJobStatus is a bounded, in-memory snapshot. Checkpoints belong in plugin
// KV when they must survive a server restart; tokens are only passed between calls.
type ActionJobStatus struct {
	Path         string    `json:"path"`
	State        string    `json:"state"` // running, done, canceled, error
	Progress     string    `json:"progress,omitempty"`
	Error        string    `json:"error,omitempty"`
	Attempts     int       `json:"attempts"`
	Started      time.Time `json:"started"`
	TotalLimitMS int64     `json:"totalLimitMs"`
	AttemptLimit int       `json:"attemptLimit"`
}

type actionJob struct {
	mu     sync.Mutex
	status ActionJobStatus
	cancel context.CancelFunc
}

func (j *actionJob) snapshot() ActionJobStatus { j.mu.Lock(); defer j.mu.Unlock(); return j.status }
func (j *actionJob) finish(state, reason string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.State, j.status.Error = state, reason
}

// startActionJob runs only owner-started work. At most two jobs run per plugin;
// at most sixteen route snapshots are retained per plugin, including running jobs.
func (m *Manager) startActionJob(pluginID string, body json.RawMessage) error {
	var out struct {
		Job string `json:"job"`
	}
	if json.Unmarshal(body, &out) != nil || out.Job == "" {
		return nil
	}
	path := out.Job
	if !strings.HasPrefix(path, "/ether/job/") {
		path = "/ether/job/" + strings.TrimPrefix(path, "/")
	}
	if len(path) > 256 || strings.Contains(path, "..") || strings.ContainsAny(path, "?#\\") {
		return nil
	}
	key := pluginID + "|" + path
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if old, ok := m.jobs.Load(key); ok && old.(*actionJob).snapshot().State == "running" {
		return nil
	}
	active, count := 0, 0
	oldestKey := ""
	var oldest time.Time
	m.jobs.Range(func(k, v any) bool {
		if !strings.HasPrefix(k.(string), pluginID+"|") {
			return true
		}
		s := v.(*actionJob).snapshot()
		count++
		if s.State == "running" {
			active++
		} else if oldestKey == "" || s.Started.Before(oldest) {
			oldestKey, oldest = k.(string), s.Started
		}
		return true
	})
	if active >= 2 {
		return fmt.Errorf("two background jobs are already running; wait for one to finish")
	}
	if _, exists := m.jobs.Load(key); !exists && count >= 16 {
		m.jobs.Delete(oldestKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.runtimeCeilings(pluginID).deadline)
	j := &actionJob{cancel: cancel, status: ActionJobStatus{Path: path, State: "running", Started: time.Now().UTC(), TotalLimitMS: m.runtimeCeilings(pluginID).deadline.Milliseconds(), AttemptLimit: ActionJobAttempts}}
	m.jobs.Store(key, j)
	go func() {
		runtime.LockOSThread() // Do not unlock: this lowered-priority thread is discarded on return.
		lowerJobPriority()
		defer cancel()
		err := runActionJob(ctx, j, ActionJobAttempts, func(ctx context.Context, token json.RawMessage) (Response, error) {
			if err := wasm.WaitBackground(ctx, m.deps.Busy); err != nil {
				return Response{}, err
			}
			payload, _ := json.Marshal(struct {
				Continuation json.RawMessage `json:"continuation,omitempty"`
			}{token})
			return m.Call(ctx, pluginID, Request{Method: "POST", Path: path, Body: payload})
		})
		if err != nil {
			state := "error"
			if ctx.Err() == context.Canceled {
				state = "canceled"
			}
			if ctx.Err() == context.DeadlineExceeded {
				err = fmt.Errorf("job reached its total time limit of %s", m.runtimeCeilings(pluginID).deadline)
			}
			reason := m.store.redactKnown(context.Background(), pluginID, err.Error())
			j.finish(state, reason)
			m.log.Warn("plugins: action job", "plugin", pluginID, "job", path, "err", reason)
		} else {
			j.finish("done", "")
		}
	}()
	return nil
}

// runActionJob treats an absent continuation flag as completion for existing
// one-call jobs. Explicit failures and non-success HTTP responses always stop.
func runActionJob(ctx context.Context, j *actionJob, attempts int, call func(context.Context, json.RawMessage) (Response, error)) error {
	var token json.RawMessage
	for n := 0; n < attempts; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("job stopped: %w", err)
		}
		j.mu.Lock()
		j.status.Attempts++
		j.mu.Unlock()
		resp, err := call(ctx, token)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("job stopped: %w", err)
		}
		if resp.Status < 200 || resp.Status >= 300 {
			return fmt.Errorf("job returned HTTP %d", resp.Status)
		}
		var step struct {
			ContinueInMS *int64          `json:"continueInMs"`
			Continuation json.RawMessage `json:"continuation"`
			Progress     string          `json:"progress"`
			Done         bool            `json:"done"`
			Failed       string          `json:"failed"`
		}
		if len(resp.Body) > 0 {
			if err = json.Unmarshal(resp.Body, &step); err != nil {
				return fmt.Errorf("invalid job response: %w", err)
			}
		}
		if len(step.Progress) > 1024 {
			return fmt.Errorf("job progress exceeds 1024 bytes")
		}
		j.mu.Lock()
		if step.Progress != "" {
			j.status.Progress = step.Progress
		}
		j.mu.Unlock()
		if step.Failed != "" {
			return fmt.Errorf("job failed: %.1024s", step.Failed)
		}
		if step.Done || step.ContinueInMS == nil {
			return nil
		}
		if *step.ContinueInMS < 0 || *step.ContinueInMS > 300000 {
			return fmt.Errorf("continueInMs must be between 0 and 300000")
		}
		if len(step.Continuation) > 64<<10 {
			return fmt.Errorf("job continuation exceeds 64 KiB")
		}
		token = step.Continuation
		if n+1 == attempts {
			break
		}
		timer := time.NewTimer(max(actionJobMinDelay, time.Duration(*step.ContinueInMS)*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("job stopped: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("job reached its limit of %d calls", attempts)
}

func (m *Manager) cancelActionJobs(id string) {
	m.jobs.Range(func(k, v any) bool {
		if id == "" || strings.HasPrefix(k.(string), id+"|") {
			v.(*actionJob).cancel()
		}
		return true
	})
}
func (m *Manager) handleActionJobs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := m.Get(r.Context(), id); err != nil {
		m.fail(w, err)
		return
	}
	jobs := []ActionJobStatus{}
	m.jobs.Range(func(k, v any) bool {
		if strings.HasPrefix(k.(string), id+"|") {
			s := v.(*actionJob).snapshot()
			s.Progress = m.store.redactKnown(r.Context(), id, s.Progress)
			jobs = append(jobs, s)
		}
		return true
	})
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Started.After(jobs[j].Started) })
	writeJSON(w, http.StatusOK, jobs)
}
func (m *Manager) handleCancelActionJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "invalid", "The request was not understood.", nil)
		return
	}
	if job, ok := m.jobs.Load(r.PathValue("id") + "|" + req.Path); ok {
		job.(*actionJob).cancel()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
