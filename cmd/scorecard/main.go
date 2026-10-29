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
	Samples     int        `json:"samples"`
	Concurrency []int      `json:"concurrency"`
	Scenarios   []scenario `json:"scenarios"`
}
type measurement struct {
	P50             float64 `json:"p50_ms"`
	P95             float64 `json:"p95_ms"`
	TTFBP50         float64 `json:"ttfb_p50_ms"`
	TTFBP95         float64 `json:"ttfb_p95_ms"`
	Errors          int     `json:"errors"`
	SchemaErrors    int     `json:"schema_errors"`
	SchemaUncovered int     `json:"schema_uncovered"`
	Samples         int     `json:"samples"`
}
type result struct {
	Name            string      `json:"name"`
	Concurrency     int         `json:"concurrency"`
	Reference       measurement `json:"reference"`
	Ether           measurement `json:"ether"`
	Winner          string      `json:"winner"`
	P50Winner       string      `json:"p50_winner"`
	P95Winner       string      `json:"p95_winner"`
	TTFBP50Winner   string      `json:"ttfb_p50_winner"`
	TTFBP95Winner   string      `json:"ttfb_p95_winner"`
	CompletionScope string      `json:"completion_scope"`
	SchemaChecked   bool        `json:"schema_checked"`
}

func measure(client *http.Client, t target, samples, concurrency int) measurement {
	return measureWithSchema(client, t, samples, concurrency, nil)
}
func measureWithSchema(client *http.Client, t target, samples, concurrency int, spec *openapi.Document) measurement {
	var mu sync.Mutex
	var durations []float64
	var firstBytes []float64
	errs := 0
	schemaErrors := 0
	schemaUncovered := 0
	jobs := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
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
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < samples; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()
	sort.Float64s(durations)
	sort.Float64s(firstBytes)
	m := measurement{Errors: errs, SchemaErrors: schemaErrors, SchemaUncovered: schemaUncovered, Samples: samples}
	if len(durations) > 0 {
		m.P50 = percentile(durations, 50)
		m.P95 = percentile(durations, 95)
		m.TTFBP50 = percentile(firstBytes, 50)
		m.TTFBP95 = percentile(firstBytes, 95)
	}
	return m
}
func percentile(v []float64, p int) float64 { return v[(len(v)*p+99)/100-1] }
func winner(a, b measurement) string {
	if a.Errors > 0 || b.Errors > 0 || a.SchemaErrors > 0 || b.SchemaErrors > 0 || a.SchemaUncovered > 0 || b.SchemaUncovered > 0 {
		return "inconclusive"
	}
	if a.P50 == b.P50 && a.P95 == b.P95 {
		return "tie"
	}
	if b.P50 <= a.P50 && b.P95 <= a.P95 {
		return "ether"
	}
	if a.P50 <= b.P50 && a.P95 <= b.P95 {
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
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
			a := measureWithSchema(client, s.Reference, c.Samples, n, spec)
			b := measureWithSchema(client, s.Ether, c.Samples, n, spec)
			out = append(out, result{Name: s.Name, Concurrency: n, Reference: a, Ether: b, Winner: winner(a, b), P50Winner: metricWinner(a.P50, b.P50, a.Errors+a.SchemaErrors+a.SchemaUncovered, b.Errors+b.SchemaErrors+b.SchemaUncovered), P95Winner: metricWinner(a.P95, b.P95, a.Errors+a.SchemaErrors+a.SchemaUncovered, b.Errors+b.SchemaErrors+b.SchemaUncovered), TTFBP50Winner: metricWinner(a.TTFBP50, b.TTFBP50, a.Errors+a.SchemaErrors+a.SchemaUncovered, b.Errors+b.SchemaErrors+b.SchemaUncovered), TTFBP95Winner: metricWinner(a.TTFBP95, b.TTFBP95, a.Errors+a.SchemaErrors+a.SchemaUncovered, b.Errors+b.SchemaErrors+b.SchemaUncovered), CompletionScope: scope, SchemaChecked: spec != nil})
		}
	}
	return out, nil
}
func main() {
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
