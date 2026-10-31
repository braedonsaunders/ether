// Scorecard measures explicitly configured, read-only HTTP requests against two servers.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type target struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Variables map[string]string `json:"variables,omitempty"`
	ReadLimit int64             `json:"read_limit,omitempty"`
	SpecPath  string            `json:"spec_path,omitempty"`
	Cgroup    string            `json:"cgroup,omitempty"`
}
type scenario struct {
	Name        string `json:"name"`
	ProfileFile string `json:"profile_file,omitempty"`
	Flow        string `json:"flow,omitempty"`
	Probe       string `json:"probe,omitempty"`
	SpecFile    string `json:"spec_file,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Reference   target `json:"reference"`
	Ether       target `json:"ether"`
}
type config struct {
	Samples       int        `json:"samples"`
	Concurrency   []int      `json:"concurrency"`
	Scenarios     []scenario `json:"scenarios"`
	RatePerSecond float64    `json:"rate_per_second,omitempty"`
	Repetitions   int        `json:"repetitions,omitempty"`
	TimeoutMS     int        `json:"timeout_ms,omitempty"`
	CacheState    string     `json:"cache_state,omitempty"`
}
type quantileInterval struct {
	Lower float64 `json:"lower_ms"`
	Upper float64 `json:"upper_ms"`
}
type measurement struct {
	P50CI95             *quantileInterval `json:"p50_ci95,omitempty"`
	P95CI95             *quantileInterval `json:"p95_ci95,omitempty"`
	P99CI95             *quantileInterval `json:"p99_ci95,omitempty"`
	TTFBP50CI95         *quantileInterval `json:"ttfb_p50_ci95,omitempty"`
	TTFBP95CI95         *quantileInterval `json:"ttfb_p95_ci95,omitempty"`
	TTFBP99CI95         *quantileInterval `json:"ttfb_p99_ci95,omitempty"`
	P50                 float64           `json:"p50_ms"`
	P95                 float64           `json:"p95_ms"`
	P99                 float64           `json:"p99_ms"`
	TTFBP50             float64           `json:"ttfb_p50_ms"`
	TTFBP95             float64           `json:"ttfb_p95_ms"`
	TTFBP99             float64           `json:"ttfb_p99_ms"`
	AllScheduledP50     float64           `json:"all_scheduled_p50_ms"`
	AllScheduledP95     float64           `json:"all_scheduled_p95_ms"`
	AllScheduledP99     float64           `json:"all_scheduled_p99_ms"`
	QueueP50            float64           `json:"queue_p50_ms"`
	QueueP95            float64           `json:"queue_p95_ms"`
	QueueP99            float64           `json:"queue_p99_ms"`
	WallSeconds         float64           `json:"wall_seconds"`
	StartedAt           time.Time         `json:"started_at"`
	EndedAt             time.Time         `json:"ended_at"`
	SuccessfulPerSecond float64           `json:"successful_per_second"`
	HTTPSuccesses       int               `json:"http_successes"`
	ValidSuccesses      int               `json:"valid_successes"`
	Resource            *resourceDelta    `json:"resource,omitempty"`
	ResourceError       string            `json:"resource_error,omitempty"`
	Timeouts            int               `json:"timeouts"`
	Errors              int               `json:"errors"`
	SchemaErrors        int               `json:"schema_errors"`
	SchemaUncovered     int               `json:"schema_uncovered"`
	Samples             int               `json:"samples"`
}
type result struct {
	Name            string      `json:"name"`
	Concurrency     int         `json:"concurrency"`
	Reference       measurement `json:"reference"`
	Ether           measurement `json:"ether"`
	Winner          string      `json:"winner"`
	P50Winner       string      `json:"p50_winner"`
	P95Winner       string      `json:"p95_winner"`
	P99Winner       string      `json:"p99_winner"`
	TTFBP50Winner   string      `json:"ttfb_p50_winner"`
	TTFBP95Winner   string      `json:"ttfb_p95_winner"`
	TTFBP99Winner   string      `json:"ttfb_p99_winner"`
	Repetition      int         `json:"repetition"`
	Order           string      `json:"order"`
	LoadModel       string      `json:"load_model"`
	RatePerSecond   float64     `json:"rate_per_second,omitempty"`
	CacheState      string      `json:"cache_state"`
	CompletionScope string      `json:"completion_scope"`
	SchemaChecked   bool        `json:"schema_checked"`
}

func measure(client *http.Client, t target, samples, concurrency int) measurement {
	return measureScheduled(client, t, samples, concurrency, nil, 0)
}
func measureWithSchema(client *http.Client, t target, samples, concurrency int, spec *openapi.Document) measurement {
	return measureScheduled(client, t, samples, concurrency, spec, 0)
}
func measureScheduled(client *http.Client, t target, samples, concurrency int, spec *openapi.Document, rate float64) measurement {
	var mu sync.Mutex
	var durations []float64
	var firstBytes []float64
	var allScheduled, queueDelays []float64
	errs := 0
	timeouts := 0
	var before *resourceSample
	resourceError := ""
	if t.Cgroup != "" {
		sample, e := readResourceSample(t.Cgroup, "/proc")
		if e != nil {
			resourceError = "resource sample unavailable"
		} else {
			before = &sample
		}
	}
	wallStart := time.Now()
	schemaErrors := 0
	schemaUncovered := 0
	type job struct{ scheduled time.Time }
	buffer := 0
	if rate > 0 {
		buffer = samples
	}
	jobs := make(chan job, buffer)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for scheduled := range jobs {
				queue := 0.0
				if !scheduled.scheduled.IsZero() {
					queue = float64(time.Since(scheduled.scheduled)) / float64(time.Millisecond)
					if queue < 0 {
						queue = 0
					}
				}
				req, err := http.NewRequest(http.MethodGet, t.URL, nil)
				if err == nil {
					for k, v := range t.Headers {
						req.Header.Set(k, v)
					}
					start := time.Now()
					var firstByte atomic.Int64
					trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { firstByte.CompareAndSwap(0, time.Since(start).Nanoseconds()+1) }}
					req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
					var resp *http.Response
					var bodyBytes bytes.Buffer
					resp, err = client.Do(req)
					if err == nil {
						var body io.Reader = resp.Body
						if t.ReadLimit > 0 {
							body = io.LimitReader(body, t.ReadLimit)
						}
						if spec != nil {
							body = io.LimitReader(body, (16<<20)+1)
							_, err = io.Copy(&bodyBytes, body)
						} else {
							_, err = io.Copy(io.Discard, body)
						}
						resp.Body.Close()
						if resp.StatusCode < 200 || resp.StatusCode >= 300 {
							err = errors.New("non-success status")
						}
					}
					elapsed := float64(time.Since(start)) / float64(time.Millisecond)
					timedOut := err != nil && os.IsTimeout(err)
					validationFailed, uncovered := false, false
					if err == nil && spec != nil {
						if bodyBytes.Len() > 16<<20 {
							validationFailed = true
						} else if e := spec.Validate(http.MethodGet, t.SpecPath, resp.StatusCode, resp.Header.Get("Content-Type"), bodyBytes.Bytes()); e != nil {
							if strings.HasPrefix(e.Error(), "no specification for ") {
								uncovered = true
							} else {
								validationFailed = true
							}
						}
					}
					mu.Lock()
					allScheduled = append(allScheduled, elapsed+queue)
					if rate > 0 {
						queueDelays = append(queueDelays, queue)
					}
					if timedOut {
						timeouts++
					}
					if validationFailed {
						schemaErrors++
					}
					if uncovered {
						schemaUncovered++
					}
					if err != nil || firstByte.Load() == 0 {
						errs++
					} else {
						durations = append(durations, elapsed)
						firstBytes = append(firstBytes, float64(firstByte.Load()-1)/float64(time.Millisecond))
					}
					mu.Unlock()
				} else {
					mu.Lock()
					errs++
					allScheduled = append(allScheduled, queue)
					if rate > 0 {
						queueDelays = append(queueDelays, queue)
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < samples; i++ {
		j := job{}
		if rate > 0 {
			j.scheduled = wallStart.Add(time.Duration(float64(i) * float64(time.Second) / rate))
			if delay := time.Until(j.scheduled); delay > 0 {
				time.Sleep(delay)
			}
		}
		jobs <- j
	}
	close(jobs)
	wg.Wait()
	sort.Float64s(durations)
	sort.Float64s(firstBytes)
	sort.Float64s(allScheduled)
	sort.Float64s(queueDelays)
	wallEnd := time.Now()
	wall := wallEnd.Sub(wallStart).Seconds()
	m := measurement{Errors: errs, Timeouts: timeouts, SchemaErrors: schemaErrors, SchemaUncovered: schemaUncovered, Samples: samples, WallSeconds: wall, StartedAt: wallStart.UTC(), EndedAt: wallEnd.UTC(), SuccessfulPerSecond: float64(len(durations)-schemaErrors-schemaUncovered) / wall, HTTPSuccesses: len(durations), ValidSuccesses: len(durations) - schemaErrors - schemaUncovered}
	if len(allScheduled) > 0 {
		m.AllScheduledP50 = percentile(allScheduled, 50)
		m.AllScheduledP95 = percentile(allScheduled, 95)
		m.AllScheduledP99 = percentile(allScheduled, 99)
	}
	if len(queueDelays) > 0 {
		m.QueueP50 = percentile(queueDelays, 50)
		m.QueueP95 = percentile(queueDelays, 95)
		m.QueueP99 = percentile(queueDelays, 99)
	}
	if t.Cgroup != "" && before != nil {
		after, e := readResourceSample(t.Cgroup, "/proc")
		if e != nil {
			resourceError = "resource sample unavailable"
		} else if delta, e := summarizeResources(*before, after, m.ValidSuccesses); e != nil {
			resourceError = "resource counter regressed"
		} else {
			m.Resource = &delta
			if before.ProcessReadErrors > 0 || after.ProcessReadErrors > 0 {
				resourceError = "process snapshot incomplete"
			}
		}
	}
	m.ResourceError = resourceError
	if len(durations) > 0 {
		m.P50 = percentile(durations, 50)
		m.P95 = percentile(durations, 95)
		m.P99 = percentile(durations, 99)
		m.P50CI95 = quantileCI(durations, .5)
		m.P95CI95 = quantileCI(durations, .95)
		m.P99CI95 = quantileCI(durations, .99)
		m.TTFBP50 = percentile(firstBytes, 50)
		m.TTFBP95 = percentile(firstBytes, 95)
		m.TTFBP99 = percentile(firstBytes, 99)
		m.TTFBP50CI95 = quantileCI(firstBytes, .5)
		m.TTFBP95CI95 = quantileCI(firstBytes, .95)
		m.TTFBP99CI95 = quantileCI(firstBytes, .99)
	}
	return m
}
func percentile(v []float64, p int) float64 { return v[(len(v)*p+99)/100-1] }

// quantileCI is a normal approximation to binomial order-statistic bounds.
// It deliberately omits intervals when either tail has too few observations.
func quantileCI(sorted []float64, p float64) *quantileInterval {
	n := float64(len(sorted))
	variance := n * p * (1 - p)
	if variance < 10 {
		return nil
	}
	center := n * p
	spread := 1.96 * math.Sqrt(variance)
	lower := int(math.Floor(center - spread))
	upper := int(math.Ceil(center + spread))
	if lower < 1 {
		lower = 1
	}
	if upper > len(sorted) {
		upper = len(sorted)
	}
	return &quantileInterval{Lower: sorted[lower-1], Upper: sorted[upper-1]}
}

func winner(a, b measurement) string {
	if a.Errors > 0 || b.Errors > 0 || a.SchemaErrors > 0 || b.SchemaErrors > 0 || a.SchemaUncovered > 0 || b.SchemaUncovered > 0 || a.ResourceError != "" || b.ResourceError != "" {
		return "inconclusive"
	}
	if a.P50 == b.P50 && a.P95 == b.P95 && a.P99 == b.P99 {
		return "tie"
	}
	if b.P50 <= a.P50 && b.P95 <= a.P95 && b.P99 <= a.P99 {
		return "ether"
	}
	if a.P50 <= b.P50 && a.P95 <= b.P95 && a.P99 <= b.P99 {
		return "reference"
	}
	return "mixed"
}
func metricWinner(reference, ether float64, refErrors, etherErrors int) string {
	if refErrors > 0 || etherErrors > 0 {
		return "inconclusive"
	}
	if reference == ether {
		return "tie"
	}
	if ether < reference {
		return "ether"
	}
	return "reference"
}
func run(c config) ([]result, error) {
	if c.Samples < 1 || len(c.Concurrency) == 0 || len(c.Scenarios) == 0 {
		return nil, errors.New("samples, concurrency and scenarios are required")
	}
	if c.Samples > 1000000 || c.Repetitions < 0 || c.Repetitions > 1000 || c.TimeoutMS < 0 || c.TimeoutMS > 3600000 || math.IsNaN(c.RatePerSecond) || math.IsInf(c.RatePerSecond, 0) || c.RatePerSecond < 0 || c.RatePerSecond > 1000000 || (c.RatePerSecond > 0 && c.RatePerSecond < 0.001) {
		return nil, errors.New("invalid load configuration")
	}
	reps := c.Repetitions
	if reps == 0 {
		reps = 1
	}
	timeout := 30 * time.Second
	if c.TimeoutMS > 0 {
		timeout = time.Duration(c.TimeoutMS) * time.Millisecond
	}
	cache := c.CacheState
	if cache == "" {
		cache = "unspecified"
	}
	model := "closed_loop"
	if c.RatePerSecond > 0 {
		model = "open_loop"
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	var out []result
	for _, s := range c.Scenarios {
		if s.ProfileFile != "" {
			var err error
			s, err = profileScenario(s)
			if err != nil {
				return nil, err
			}
		}
		if s.Name == "" || s.Reference.URL == "" || s.Ether.URL == "" {
			return nil, errors.New("scenario requires name and both URLs")
		}
		if (s.Reference.Cgroup == "") != (s.Ether.Cgroup == "") {
			return nil, errors.New("resource comparison requires both cgroups")
		}
		if s.Reference.Cgroup != "" {
			if err := validateMatchedLimits(s.Reference.Cgroup, s.Ether.Cgroup); err != nil {
				return nil, err
			}
		}
		var spec *openapi.Document
		if s.SpecFile != "" {
			data, err := os.ReadFile(s.SpecFile)
			if err != nil {
				return nil, errors.New("cannot read pinned specification")
			}
			if err = validatePinnedSpec(s.Protocol, data); err != nil {
				return nil, err
			}
			spec, err = openapi.Parse(data)
			if err != nil {
				return nil, err
			}
			if s.Reference.SpecPath == "" || s.Ether.SpecPath == "" || s.Reference.ReadLimit != 0 || s.Ether.ReadLimit != 0 {
				return nil, errors.New("schema validation requires full responses and both spec paths")
			}
			for _, path := range []string{s.Reference.SpecPath, s.Ether.SpecPath} {
				u, err := url.Parse(path)
				if err != nil {
					return nil, errors.New("invalid schema path")
				}
				for _, ext := range []string{".m3u8", ".ts", ".m4s"} {
					if strings.HasSuffix(strings.ToLower(u.Path), ext) {
						return nil, errors.New("streaming segments cannot be schema-validated")
					}
				}
			}
		}
		if s.Reference.ReadLimit < 0 || s.Reference.ReadLimit != s.Ether.ReadLimit {
			return nil, errors.New("both targets require the same non-negative read limit")
		}
		scope := "full_response"
		if s.Reference.ReadLimit > 0 {
			scope = fmt.Sprintf("up_to_%d_bytes", s.Reference.ReadLimit)
		}
		for _, n := range c.Concurrency {
			if n < 1 {
				return nil, errors.New("concurrency must be positive")
			}
			for rep := 0; rep < reps; rep++ {
				var a, b measurement
				order := "reference_first"
				if rep%2 == 0 {
					a = measureScheduled(client, s.Reference, c.Samples, n, spec, c.RatePerSecond)
					b = measureScheduled(client, s.Ether, c.Samples, n, spec, c.RatePerSecond)
				} else {
					order = "ether_first"
					b = measureScheduled(client, s.Ether, c.Samples, n, spec, c.RatePerSecond)
					a = measureScheduled(client, s.Reference, c.Samples, n, spec, c.RatePerSecond)
				}
				ae, be := a.Errors+a.SchemaErrors+a.SchemaUncovered, b.Errors+b.SchemaErrors+b.SchemaUncovered
				if a.ResourceError != "" {
					ae++
				}
				if b.ResourceError != "" {
					be++
				}
				out = append(out, result{Name: s.Name, Concurrency: n, Reference: a, Ether: b, Winner: winner(a, b), P50Winner: metricWinner(a.P50, b.P50, ae, be), P95Winner: metricWinner(a.P95, b.P95, ae, be), P99Winner: metricWinner(a.P99, b.P99, ae, be), TTFBP50Winner: metricWinner(a.TTFBP50, b.TTFBP50, ae, be), TTFBP95Winner: metricWinner(a.TTFBP95, b.TTFBP95, ae, be), TTFBP99Winner: metricWinner(a.TTFBP99, b.TTFBP99, ae, be), CompletionScope: scope, SchemaChecked: spec != nil, Repetition: rep + 1, Order: order, LoadModel: model, RatePerSecond: c.RatePerSecond, CacheState: cache})
			}
		}
	}
	return out, nil
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "workload" {
		if err := workload(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "acceptance" {
		if err := acceptance(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "resources" {
		if err := resources(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "inventory" {
		if err := inventory(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "soak" {
		if err := soak(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "soak-eval" {
		if err := soakEval(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	file := flag.String("config", os.Getenv("SCORECARD_CONFIG"), "private JSON configuration file")
	flag.Parse()
	if *file == "" {
		fmt.Fprintln(os.Stderr, "set SCORECARD_CONFIG or -config")
		os.Exit(2)
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var c config
	if err = json.Unmarshal(data, &c); err == nil {
		var results []result
		results, err = run(c)
		if err == nil {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			err = enc.Encode(results)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.ReplaceAll(err.Error(), "\n", " "))
		os.Exit(1)
	}
}
