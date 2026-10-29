// Scorecard measures explicitly configured, read-only HTTP requests against two servers.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type target struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}
type scenario struct {
	Name      string `json:"name"`
	Reference target `json:"reference"`
	Ether     target `json:"ether"`
}
type config struct {
	Samples     int        `json:"samples"`
	Concurrency []int      `json:"concurrency"`
	Scenarios   []scenario `json:"scenarios"`
}
type measurement struct {
	P50     float64 `json:"p50_ms"`
	P95     float64 `json:"p95_ms"`
	Errors  int     `json:"errors"`
	Samples int     `json:"samples"`
}
type result struct {
	Name        string      `json:"name"`
	Concurrency int         `json:"concurrency"`
	Reference   measurement `json:"reference"`
	Ether       measurement `json:"ether"`
	Winner      string      `json:"winner"`
}

func measure(client *http.Client, t target, samples, concurrency int) measurement {
	var mu sync.Mutex
	var durations []float64
	errs := 0
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
					var resp *http.Response
					resp, err = client.Do(req)
					if err == nil {
						_, err = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode < 200 || resp.StatusCode >= 300 {
							err = errors.New("non-success status")
						}
					}
					elapsed := float64(time.Since(start)) / float64(time.Millisecond)
					mu.Lock()
					if err != nil {
						errs++
					} else {
						durations = append(durations, elapsed)
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
	m := measurement{Errors: errs, Samples: samples}
	if len(durations) > 0 {
		m.P50 = percentile(durations, 50)
		m.P95 = percentile(durations, 95)
	}
	return m
}
func percentile(v []float64, p int) float64 { return v[(len(v)*p+99)/100-1] }
func winner(a, b measurement) string {
	if a.Errors > 0 || b.Errors > 0 {
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
func run(c config) ([]result, error) {
	if c.Samples < 1 || len(c.Concurrency) == 0 || len(c.Scenarios) == 0 {
		return nil, errors.New("samples, concurrency and scenarios are required")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	var out []result
	for _, s := range c.Scenarios {
		if s.Name == "" || s.Reference.URL == "" || s.Ether.URL == "" {
			return nil, errors.New("scenario requires name and both URLs")
		}
		for _, n := range c.Concurrency {
			if n < 1 {
				return nil, errors.New("concurrency must be positive")
			}
			a := measure(client, s.Reference, c.Samples, n)
			b := measure(client, s.Ether, c.Samples, n)
			out = append(out, result{s.Name, n, a, b, winner(a, b)})
		}
	}
	return out, nil
}
func main() {
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
