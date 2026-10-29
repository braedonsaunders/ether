package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type workloadTarget struct {
	BaseURL   string            `json:"base_url"`
	Cgroup    string            `json:"cgroup"`
	Variables map[string]string `json:"variables"`
}
type workloadStep struct {
	Kind                string            `json:"kind"`
	Method              string            `json:"method"`
	Path                string            `json:"path,omitempty"`
	URLVariable         string            `json:"url_variable,omitempty"`
	Headers             map[string]string `json:"headers,omitempty"`
	JSONBody            json.RawMessage   `json:"json_body,omitempty"`
	ExpectedStatus      int               `json:"expected_status"`
	Capture             map[string]string `json:"capture,omitempty"`
	Assert              map[string]string `json:"assert,omitempty"`
	MaxBytes            int64             `json:"max_bytes,omitempty"`
	CancelAfter         int64             `json:"cancel_after_bytes,omitempty"`
	ExpectedSHA256      string            `json:"expected_sha256,omitempty"`
	ExpectedDurationMS  int64             `json:"expected_duration_ms,omitempty"`
	DurationToleranceMS int64             `json:"duration_tolerance_ms,omitempty"`
}
type workloadFlow struct {
	RequiresMedia bool           `json:"requires_media"`
	Steps         []workloadStep `json:"steps"`
}
type workloadArrival struct {
	Client string `json:"client"`
	Flow   string `json:"flow"`
	AtMS   int64  `json:"at_ms"`
}
type workloadConfig struct {
	ReservationID  string                  `json:"reservation_id"`
	Reference      workloadTarget          `json:"reference"`
	Ether          workloadTarget          `json:"ether"`
	Flows          map[string]workloadFlow `json:"flows"`
	Trace          []workloadArrival       `json:"trace"`
	Concurrency    int                     `json:"concurrency"`
	Repetitions    int                     `json:"repetitions"`
	TimeoutMS      int                     `json:"timeout_ms"`
	FFmpeg         string                  `json:"ffmpeg"`
	FFmpegSHA256   string                  `json:"ffmpeg_sha256"`
	ManifestSHA256 string                  `json:"manifest_sha256"`
	CacheState     string                  `json:"cache_state"`
}
type workloadEvent struct {
	HTTPStatus         int     `json:"http_status,omitempty"`
	Client             string  `json:"client"`
	Flow               string  `json:"flow"`
	ScheduledMS        int64   `json:"scheduled_ms"`
	QueueMS            float64 `json:"queue_ms"`
	ElapsedMS          float64 `json:"elapsed_ms"`
	Completed          bool    `json:"completed"`
	Cancelled          bool    `json:"cancelled"`
	TimedOut           bool    `json:"timed_out"`
	CleanupFailures    int     `json:"cleanup_failures"`
	CleanupTimeouts    int     `json:"cleanup_timeouts"`
	VerifiedDurationMS int64   `json:"verified_duration_ms"`
	Error              string  `json:"error,omitempty"`
}
type workloadRun struct {
	Complete                          bool            `json:"complete"`
	Dropped                           int             `json:"dropped"`
	Target                            string          `json:"target"`
	Repetition                        int             `json:"repetition"`
	Order                             string          `json:"order"`
	StartedAt                         time.Time       `json:"started_at"`
	EndedAt                           time.Time       `json:"ended_at"`
	Offered                           int             `json:"offered"`
	Started                           int             `json:"started"`
	Completed                         int             `json:"completed"`
	Cancelled                         int             `json:"cancelled"`
	Failed                            int             `json:"failed"`
	Timeouts                          int             `json:"timeouts"`
	CleanupFailures                   int             `json:"cleanup_failures"`
	VerifiedDurationMS                int64           `json:"verified_duration_ms"`
	CPUSecondsPerVerifiedStreamMinute *float64        `json:"cpu_seconds_per_verified_stream_minute,omitempty"`
	Resource                          *resourceDelta  `json:"resource,omitempty"`
	ResourceError                     string          `json:"resource_error,omitempty"`
	Events                            []workloadEvent `json:"events"`
	AllP50MS                          float64         `json:"all_scheduled_p50_ms"`
	AllP95MS                          float64         `json:"all_scheduled_p95_ms"`
	AllP99MS                          float64         `json:"all_scheduled_p99_ms"`
	QueueP50MS                        float64         `json:"queue_p50_ms"`
	QueueP95MS                        float64         `json:"queue_p95_ms"`
	QueueP99MS                        float64         `json:"queue_p99_ms"`
}
type mediaVerifier func(context.Context, string) (int64, error)

func workload(args []string) error {
	fs := flag.NewFlagSet("workload", flag.ContinueOnError)
	path := fs.String("config", os.Getenv("SCORECARD_WORKLOAD_CONFIG"), "private isolated-workload JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("workload config required")
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return errors.New("cannot read workload config")
	}
	var cfg workloadConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		return errors.New("invalid workload config")
	}
	if err = validateWorkload(cfg); err != nil {
		return err
	}
	if err = verifyFileHash(cfg.FFmpeg, cfg.FFmpegSHA256); err != nil {
		return err
	}
	if err = validateMatchedLimits(cfg.Reference.Cgroup, cfg.Ether.Cgroup); err != nil {
		return err
	}
	if err = validateGeneratorIsolation(cfg.Reference.Cgroup, cfg.Ether.Cgroup); err != nil {
		return err
	}
	verifier := func(ctx context.Context, path string) (int64, error) { return decodeDuration(ctx, cfg.FFmpeg, path) }
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runs := []workloadRun{}
	for n := 0; n < cfg.Repetitions; n++ {
		order := "reference_first"
		targets := []struct {
			name   string
			target workloadTarget
		}{{"reference", cfg.Reference}, {"ether", cfg.Ether}}
		if n%2 == 1 {
			order = "ether_first"
			targets[0], targets[1] = targets[1], targets[0]
		}
		for _, t := range targets {
			run := executeWorkload(ctx, cfg, t.name, t.target, verifier)
			run.Repetition = n + 1
			run.Order = order
			runs = append(runs, run)
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	out := struct {
		Reservation    string        `json:"reservation_id"`
		ManifestSHA256 string        `json:"manifest_sha256"`
		FFmpegSHA256   string        `json:"ffmpeg_sha256"`
		CacheState     string        `json:"cache_state"`
		Caveat         string        `json:"caveat"`
		Runs           []workloadRun `json:"runs"`
	}{cfg.ReservationID, cfg.ManifestSHA256, cfg.FFmpegSHA256, cfg.CacheState, "Staging, media provenance, fault windows and quality settings require independent verification; this command does not assert an efficiency winner or inject provider faults.", runs}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
func validateWorkload(c workloadConfig) error {
	if c.ReservationID == "" || c.Concurrency < 1 || c.Concurrency > 128 || c.Repetitions < 1 || c.Repetitions > 100 || c.TimeoutMS < 1 || c.TimeoutMS > 3600000 || len(c.Trace) == 0 || len(c.Trace) > 1000000 || c.FFmpeg == "" || !filepath.IsAbs(c.FFmpeg) || !validSHA(c.FFmpegSHA256) || !validSHA(c.ManifestSHA256) || (c.CacheState != "warm" && c.CacheState != "cold") {
		return errors.New("reservation, bounded workload, timeout and absolute ffmpeg are required")
	}
	for _, t := range []workloadTarget{c.Reference, c.Ether} {
		u, e := url.Parse(t.BaseURL)
		if e != nil || u.Scheme != "http" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("target must be an HTTP loopback base")
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return errors.New("target must be an HTTP loopback base")
		}
	}
	if c.Reference.BaseURL == c.Ether.BaseURL {
		return errors.New("targets must be distinct")
	}
	for _, a := range c.Trace {
		if a.Client == "" || a.AtMS < 0 || a.AtMS > int64((24*time.Hour)/time.Millisecond) {
			return errors.New("invalid workload arrival")
		}
		flow, ok := c.Flows[a.Flow]
		if !ok || len(flow.Steps) == 0 {
			return errors.New("missing workload flow")
		}
		hasCleanup, hasMedia, hasPlayback, hasCancel := false, false, false, false
		for _, s := range flow.Steps {
			switch s.Kind {
			case "auth", "browse", "play", "seek", "subtitle", "cancel", "media", "cleanup":
			default:
				return errors.New("invalid workload step")
			}
			if s.Kind == "play" || s.Kind == "seek" {
				hasPlayback = true
			}
			if s.Kind == "cancel" {
				hasCancel = true
			}
			if s.Kind == "cleanup" {
				hasCleanup = true
			}
			if s.Kind == "media" {
				refHash, e1 := expandProfileHeader(s.ExpectedSHA256, c.Reference.Variables)
				etherHash, e2 := expandProfileHeader(s.ExpectedSHA256, c.Ether.Variables)
				if e1 != nil || e2 != nil || !validSHA(refHash) || !strings.EqualFold(refHash, etherHash) {
					return errors.New("media verification requires matching predeclared delivered-byte hashes")
				}
				hasMedia = true
				if s.ExpectedDurationMS <= 0 || s.DurationToleranceMS < 0 || s.ExpectedSHA256 == "" {
					return errors.New("media verification requires expected duration and SHA-256")
				}
			}
			if s.ExpectedStatus < 200 || s.ExpectedStatus >= 300 || s.MaxBytes < 1 || s.MaxBytes > 512<<20 || ((s.Path == "") == (s.URLVariable == "")) {
				return errors.New("invalid workload step constraints")
			}
			switch s.Method {
			case "GET", "POST", "DELETE":
			default:
				return errors.New("unsupported workload method")
			}
			if s.Kind != "media" && s.Kind != "cancel" && s.MaxBytes > 4<<20 {
				return errors.New("non-media response limit exceeds 4 MiB")
			}
			if s.Kind == "cancel" && (s.CancelAfter < 1 || s.CancelAfter > s.MaxBytes) {
				return errors.New("cancel requires bounded bytes")
			}
		}
		if !hasCleanup || flow.RequiresMedia && !hasMedia || hasPlayback && !flow.RequiresMedia && !hasCancel {
			return errors.New("flow requires cleanup and declared media verification")
		}
	}
	return nil
}
func validateGeneratorIsolation(groups ...string) error {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return errors.New("cannot verify generator cgroup")
	}
	current := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			current = filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::"))
			break
		}
	}
	if current == "" {
		return errors.New("cgroup v2 generator isolation unavailable")
	}
	if filepath.Clean(current) == "/sys/fs/cgroup" {
		return errors.New("generator cgroup namespace is ambiguous; run from the host")
	}

	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return errors.New("cannot verify generator cgroup")
	}
	for _, group := range groups {
		resolved, err := filepath.EvalSymlinks(group)
		if err != nil {
			return errors.New("cannot verify server cgroup")
		}
		rootRel, e := filepath.Rel("/sys/fs/cgroup", resolved)
		if e != nil || rootRel == ".." || strings.HasPrefix(rootRel, "../") {
			return errors.New("server cgroups must be under host cgroup v2 root")
		}
		relative, err := filepath.Rel(resolved, current)
		if err != nil {
			return errors.New("cannot verify generator cgroup")
		}
		if relative == "." || relative != ".." && !strings.HasPrefix(relative, "../") {
			return errors.New("generator must be outside server cgroups")
		}
	}
	return nil
}
func decodeDuration(ctx context.Context, ffmpeg, path string) (int64, error) {

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-xerror", "-i", path, "-map", "0:v:0?", "-map", "0:a:0?", "-progress", "pipe:1", "-nostats", "-f", "null", "-")
	out, err := cmd.Output()
	if err != nil {
		return 0, errors.New("media decode failed")
	}
	var micros int64
	for _, line := range strings.Split(string(out), "\n") {
		if value, ok := strings.CutPrefix(line, "out_time_us="); ok {
			if n, e := strconv.ParseInt(value, 10, 64); e == nil && n > micros {
				micros = n
			}
		}
	}
	if micros <= 0 {
		return 0, errors.New("decoded duration unavailable")
	}
	return micros / 1000, nil
}

type workloadJob struct {
	Arrival   workloadArrival
	Scheduled time.Time
}

func executeWorkload(ctx context.Context, c workloadConfig, name string, target workloadTarget, verify mediaVerifier) workloadRun {
	out := workloadRun{Target: name, Offered: len(c.Trace), Events: make([]workloadEvent, 0, len(c.Trace)), Complete: true}
	before, beforeErr := readResourceSample(target.Cgroup, "/proc")
	if beforeErr != nil {
		out.ResourceError = "resource sample unavailable"
	}
	out.StartedAt = time.Now().UTC()
	base := time.Now()
	trace := append([]workloadArrival(nil), c.Trace...)
	sort.SliceStable(trace, func(i, j int) bool { return trace[i].AtMS < trace[j].AtMS })
	locks := map[string]*sync.Mutex{}
	for _, arrival := range trace {
		if locks[arrival.Client] == nil {
			locks[arrival.Client] = &sync.Mutex{}
		}
	}
	jobs := make(chan workloadJob, len(trace))
	client := &http.Client{Timeout: time.Duration(c.TimeoutMS) * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < c.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				lock := locks[j.Arrival.Client]
				lock.Lock()
				start := time.Now()
				queue := float64(start.Sub(j.Scheduled)) / float64(time.Millisecond)
				if queue < 0 {
					queue = 0
				}
				event := runWorkloadFlow(ctx, client, c, target, j.Arrival, verify)
				event.QueueMS = queue
				event.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
				lock.Unlock()
				mu.Lock()
				out.Events = append(out.Events, event)
				mu.Unlock()
			}
		}()
	}
	produced := 0
schedule:
	for _, arrival := range trace {
		if ctx.Err() != nil {
			out.Complete = false
			out.ResourceError = "workload interrupted"
			break
		}
		scheduled := base.Add(time.Duration(arrival.AtMS) * time.Millisecond)
		if delay := time.Until(scheduled); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				out.Complete = false
				out.ResourceError = "workload interrupted"
				break schedule
			case <-timer.C:
			}
		}
		jobs <- workloadJob{Arrival: arrival, Scheduled: scheduled}
		produced++
	}
	out.Dropped = out.Offered - produced
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		out.Complete = false
		out.ResourceError = "workload interrupted"
	}
	out.EndedAt = time.Now().UTC()
	sort.SliceStable(out.Events, func(i, j int) bool {
		if out.Events[i].ScheduledMS == out.Events[j].ScheduledMS {
			return out.Events[i].Client < out.Events[j].Client
		}
		return out.Events[i].ScheduledMS < out.Events[j].ScheduledMS
	})
	out.Started = len(out.Events)
	for _, event := range out.Events {
		if event.TimedOut {
			out.Timeouts++
		}
		out.Timeouts += event.CleanupTimeouts
		out.CleanupFailures += event.CleanupFailures
		if event.Error != "" || event.CleanupFailures > 0 {
			out.Failed++
		} else if event.Cancelled {
			out.Cancelled++
		} else if event.Completed {
			out.Completed++
		}
		out.VerifiedDurationMS += event.VerifiedDurationMS
	}
	scheduledLatencies, queues := make([]float64, 0, len(out.Events)), make([]float64, 0, len(out.Events))
	for _, e := range out.Events {
		scheduledLatencies = append(scheduledLatencies, e.QueueMS+e.ElapsedMS)
		queues = append(queues, e.QueueMS)
	}
	sort.Float64s(scheduledLatencies)
	sort.Float64s(queues)
	if len(queues) > 0 {
		out.AllP50MS = percentile(scheduledLatencies, 50)
		out.AllP95MS = percentile(scheduledLatencies, 95)
		out.AllP99MS = percentile(scheduledLatencies, 99)
		out.QueueP50MS = percentile(queues, 50)
		out.QueueP95MS = percentile(queues, 95)
		out.QueueP99MS = percentile(queues, 99)
	}
	if beforeErr == nil {
		after, err := readResourceSample(target.Cgroup, "/proc")
		if err != nil {
			out.ResourceError = "resource sample unavailable"
		} else if delta, err := summarizeResources(before, after, out.Completed); err != nil {
			out.ResourceError = "resource counter regressed"
		} else {
			out.Resource = &delta
			if before.ProcessReadErrors > 0 || after.ProcessReadErrors > 0 || before.ProcessCount < 1 || after.ProcessCount < 1 {
				out.ResourceError = "process snapshot incomplete"
			}
			if out.Complete && out.Dropped == 0 && out.ResourceError == "" && out.Failed == 0 && out.Timeouts == 0 && out.CleanupFailures == 0 && out.VerifiedDurationMS > 0 {
				v := delta.CPUSeconds / (float64(out.VerifiedDurationMS) / 60000)
				out.CPUSecondsPerVerifiedStreamMinute = &v
			}
		}
	}
	return out
}
func runWorkloadFlow(parent context.Context, client *http.Client, c workloadConfig, target workloadTarget, arrival workloadArrival, verify mediaVerifier) workloadEvent {
	event := workloadEvent{Client: arrival.Client, Flow: arrival.Flow, ScheduledMS: arrival.AtMS}
	flow := c.Flows[arrival.Flow]
	vars := map[string]string{}
	for k, v := range target.Variables {
		vars[k] = v
	}
	vars["device_id"] = arrival.Client
	vars["client_id"] = arrival.Client
	ctx, cancel := context.WithTimeout(parent, time.Duration(c.TimeoutMS)*time.Millisecond)
	defer cancel()
	verified := false
	cleanup := []workloadStep{}
	for _, step := range flow.Steps {
		if step.Kind == "cleanup" {
			cleanup = append(cleanup, step)
			continue
		}
		if event.Error != "" {
			continue
		}
		duration, cancelled, err := executeWorkloadStep(ctx, client, target, step, vars, verify)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
				event.TimedOut = true
				event.Error = "timeout"
			} else {
				var status *workloadStatusError
				if errors.As(err, &status) {
					event.HTTPStatus = status.Code
					event.Error = "unexpected_http_status"
				} else if errors.Is(err, context.Canceled) {
					event.Error = "interrupted"
				} else {
					event.Error = err.Error()
				}
			}
			continue
		}
		if duration > 0 {
			verified = true
			event.VerifiedDurationMS += duration
		}
		event.Cancelled = event.Cancelled || cancelled
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Duration(c.TimeoutMS)*time.Millisecond)
	defer cleanupCancel()
	for _, step := range cleanup {
		if _, _, err := executeWorkloadStep(cleanupCtx, client, target, step, vars, verify); err != nil {
			event.CleanupFailures++
			if os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
				event.CleanupTimeouts++
			}
		}
	}
	if flow.RequiresMedia && !verified && event.Error == "" {
		event.Error = "media_not_verified"
	}
	event.Completed = event.Error == "" && event.CleanupFailures == 0
	return event
}
func executeWorkloadStep(ctx context.Context, client *http.Client, target workloadTarget, step workloadStep, vars map[string]string, verify mediaVerifier) (int64, bool, error) {
	requestURL, err := workloadURL(target.BaseURL, step, vars)
	if err != nil {
		return 0, false, errors.New("invalid_step_url")
	}
	var body io.Reader
	if len(step.JSONBody) > 0 {
		var value any
		if json.Unmarshal(step.JSONBody, &value) != nil {
			return 0, false, errors.New("invalid_step_body")
		}
		value, err = expandJSON(value, vars)
		if err != nil {
			return 0, false, errors.New("missing_step_variable")
		}
		encoded, e := json.Marshal(value)
		if e != nil {
			return 0, false, errors.New("invalid_step_body")
		}
		body = bytes.NewReader(encoded)
	}
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, step.Method, requestURL, body)
	if err != nil {
		return 0, false, errors.New("invalid_step_request")
	}
	for k, v := range step.Headers {
		expanded, e := expandProfileHeader(v, vars)
		if e != nil {
			return 0, false, errors.New("missing_step_variable")
		}
		req.Header.Set(k, expanded)
	}
	resp, err := client.Do(req)
	if err != nil {
		if os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
			return 0, false, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return 0, false, context.Canceled
		}
		return 0, false, errors.New("http_request_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != step.ExpectedStatus {
		return 0, false, &workloadStatusError{Code: resp.StatusCode}
	}
	if step.Kind == "cancel" {
		n, e := io.CopyN(io.Discard, resp.Body, step.CancelAfter)
		cancel()
		if e != nil || n != step.CancelAfter {
			if os.IsTimeout(e) || errors.Is(e, context.DeadlineExceeded) {
				return 0, false, context.DeadlineExceeded
			}
			if errors.Is(e, context.Canceled) {
				return 0, false, context.Canceled
			}
			return 0, false, errors.New("cancellation_read_failed")
		}
		return 0, true, nil
	}
	if step.Kind == "media" {
		return verifyWorkloadMedia(ctx, resp.Body, step, vars, verify)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, step.MaxBytes+1))
	if err != nil {
		if os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
			return 0, false, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return 0, false, context.Canceled
		}
		return 0, false, errors.New("http_read_failed")
	}
	if int64(len(data)) > step.MaxBytes {
		return 0, false, errors.New("response_too_large")
	}
	if len(step.Capture) > 0 || len(step.Assert) > 0 {
		var value any
		if json.Unmarshal(data, &value) != nil {
			return 0, false, errors.New("invalid_step_json")
		}
		for pointer, expected := range step.Assert {
			actual, ok := jsonPointerString(value, pointer)
			if !ok {
				return 0, false, errors.New("missing_assertion_field")
			}
			want, e := expandProfileHeader(expected, vars)
			if e != nil || actual != want {
				return 0, false, errors.New("assertion_failed")
			}
		}
		for key, pointer := range step.Capture {
			value, ok := jsonPointerString(value, pointer)
			if !ok {
				return 0, false, errors.New("missing_capture_field")
			}
			vars[key] = value
		}
	}
	return 0, false, nil
}
func workloadURL(base string, step workloadStep, vars map[string]string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	var raw string
	if step.URLVariable != "" {
		var ok bool
		raw, ok = vars[step.URLVariable]
		if !ok {
			return "", errors.New("missing url")
		}
	} else {
		raw, err = expandProfilePath(step.Path, vars)
		if err != nil {
			return "", err
		}
	}
	v, err := url.Parse(raw)
	if err != nil || v.User != nil || v.Fragment != "" {
		return "", errors.New("invalid url")
	}
	if v.IsAbs() {
		if v.Scheme != u.Scheme || v.Host != u.Host {
			return "", errors.New("url escaped target")
		}
		return v.String(), nil
	}
	if !strings.HasPrefix(v.Path, "/") || strings.HasPrefix(v.Path, "//") {
		return "", errors.New("invalid url")
	}
	if step.URLVariable == "" {
		return strings.TrimRight(base, "/") + v.String(), nil
	}
	u.Path = v.Path
	u.RawPath = v.RawPath
	u.RawQuery = v.RawQuery
	return u.String(), nil
}
func expandJSON(value any, vars map[string]string) (any, error) {
	switch v := value.(type) {
	case string:
		return expandProfileHeader(v, vars)
	case []any:
		for i := range v {
			x, e := expandJSON(v[i], vars)
			if e != nil {
				return nil, e
			}
			v[i] = x
		}
	case map[string]any:
		for k, x := range v {
			y, e := expandJSON(x, vars)
			if e != nil {
				return nil, e
			}
			v[k] = y
		}
	}
	return value, nil
}
func jsonPointerString(value any, pointer string) (string, bool) {
	if pointer == "" || !strings.HasPrefix(pointer, "/") {
		return "", false
	}
	for _, raw := range strings.Split(pointer[1:], "/") {
		key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[key]
			if !ok {
				return "", false
			}
		case []any:
			i, e := strconv.Atoi(key)
			if e != nil || i < 0 || i >= len(v) {
				return "", false
			}
			value = v[i]
		default:
			return "", false
		}
	}
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		return "", false
	}
}

type workloadStatusError struct{ Code int }

func (e *workloadStatusError) Error() string { return "unexpected_http_status" }
func verifyWorkloadMedia(ctx context.Context, body io.Reader, step workloadStep, vars map[string]string, verify mediaVerifier) (int64, bool, error) {
	expected, err := expandProfileHeader(step.ExpectedSHA256, vars)
	if err != nil || !validSHA(expected) {
		return 0, false, errors.New("missing_expected_media_hash")
	}
	f, err := os.CreateTemp("", "scorecard-media-*")
	if err != nil {
		return 0, false, errors.New("media_temp_unavailable")
	}
	path := f.Name()
	defer os.Remove(path)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, step.MaxBytes+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		if os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
			return 0, false, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return 0, false, context.Canceled
		}
		return 0, false, errors.New("media_read_failed")
	}
	if n > step.MaxBytes {
		return 0, false, errors.New("response_too_large")
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), expected) {
		return 0, false, errors.New("media_hash_mismatch")
	}
	duration, err := verify(ctx, path)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, false, context.DeadlineExceeded
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return 0, false, context.Canceled
		}
		return 0, false, errors.New("media_decode_failed")
	}
	difference := duration - step.ExpectedDurationMS
	if difference < 0 {
		difference = -difference
	}
	if difference > step.DurationToleranceMS {
		return 0, false, errors.New("media_duration_mismatch")
	}
	return duration, false, nil
}

func verifyFileHash(path, want string) error {
	f, e := os.Open(path)
	if e != nil {
		return errors.New("ffmpeg binary unavailable")
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return errors.New("ffmpeg binary unreadable")
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want) {
		return errors.New("ffmpeg digest mismatch")
	}
	return nil
}
