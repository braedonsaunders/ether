// conformdiff records and compares Plex Media Server HTTP responses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	pmsconform "github.com/braedonsaunders/ether/internal/conform"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: conformdiff record|compare|live [flags]")
	}
	cmd := args[0]
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	ref := f.String("reference", "", "fixture directory")
	refServer := f.String("reference-server", "", "live reference PMS URL")
	target := f.String("target", "", "target PMS URL")
	server := f.String("server", "", "PMS URL to record")
	token := f.String("token", "", "PMS access token")
	out := f.String("out", "testdata/conform/plex", "output fixture directory")
	client := f.String("client", "all", "web|ios|appletv|all")
	format := f.String("format", "text", "text|json")
	summary := f.Bool("summary", false, "print per-endpoint missing/extra/type differences and a count table")
	values := f.Bool("values", false, "include informational value differences")
	timeout := f.Duration("timeout", 90*time.Second, "per-request timeout")
	concurrency := f.Int("concurrency", 2, "maximum concurrent requests")
	only := f.String("only", "", "only requests whose path contains this substring (record keeps the saved discovery IDs)")
	ignore := f.String("ignore", "", "drop differences whose schema path matches this regexp (known data gaps)")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	if *timeout <= 0 || *concurrency < 1 {
		return fmt.Errorf("--timeout and --concurrency must be positive")
	}
	var report pmsconform.Report
	var err error
	switch cmd {
	case "record":
		if *server == "" {
			return fmt.Errorf("record requires --server")
		}
		report, err = record(*server, *token, *out, *client, *only, *timeout, *concurrency)
	case "compare":
		if *target == "" || *ref == "" {
			return fmt.Errorf("compare requires --reference and --target")
		}
		report, err = compare(*ref, *target, *token, *client, *only, *timeout, *concurrency)
	case "live":
		if *refServer == "" || *target == "" {
			return fmt.Errorf("live requires --reference-server and --target")
		}
		tmp, e := os.MkdirTemp("", "conformdiff-reference-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(tmp)
		report, err = record(*refServer, *token, tmp, *client, *only, *timeout, *concurrency)
		if err == nil {
			var targetReport pmsconform.Report
			targetReport, err = compare(tmp, *target, *token, *client, *only, *timeout, *concurrency)
			report = mergeReports(report, targetReport)
		}
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		return err
	}
	if *ignore != "" {
		re, e := regexp.Compile(*ignore)
		if e != nil {
			return fmt.Errorf("--ignore: %w", e)
		}
		report = filterReport(report, re)
	}
	emit(os.Stdout, report, *format, *summary, *values)
	if n := report.Blocking(); n > 0 {
		return fmt.Errorf("report contains %d errors or blocking differences", n)
	}
	return nil
}

// filterReport drops schema differences whose path matches re (request
// errors, status and header differences are always kept).
func filterReport(r pmsconform.Report, re *regexp.Regexp) pmsconform.Report {
	kept := r.Differences[:0:0]
	for _, d := range r.Differences {
		switch d.Kind {
		case pmsconform.KindMissing, pmsconform.KindExtra, pmsconform.KindType, pmsconform.KindValue:
			if re.MatchString(d.Path) {
				continue
			}
		}
		kept = append(kept, d)
	}
	r.Differences = kept
	return r
}

func profiles(sel string) []string {
	if sel == "all" || sel == "" {
		return []string{"web", "ios", "appletv"}
	}
	return strings.Split(sel, ",")
}

// corpus is the probe corpus, optionally narrowed to paths containing only.
func corpus(only string) []pmsconform.Request {
	all := conformplex.Corpus()
	if only == "" {
		return all
	}
	var out []pmsconform.Request
	for _, r := range all {
		if strings.Contains(r.Path, only) {
			out = append(out, r)
		}
	}
	return out
}

func record(base, token, out, sel, only string, timeout time.Duration, concurrency int) (pmsconform.Report, error) {
	client := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	report := pmsconform.Report{}
	var mu sync.Mutex
	for _, profile := range profiles(sel) {
		// A partial recording reuses the saved IDs so the new fixtures
		// describe the same items as the rest of the set.
		ids, e := loadDiscovery(out, profile)
		if only == "" || e != nil {
			ids = conformplex.Discover(ctx, client, base, token, profile, nil)
			for _, w := range ids.Warnings {
				report.Discovery = append(report.Discovery, profile+": "+w)
			}
			if e := saveDiscovery(out, profile, ids); e != nil {
				return report, e
			}
		}
		type job struct {
			req  pmsconform.Request
			json bool
		}
		jobs := make(chan job)
		var wg sync.WaitGroup
		var fixtureMu sync.Mutex
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					resp, e := conformplex.Fetch(ctx, client, base, token, j.req, profile, ids.Values, j.json)
					filename := fixtureName(profile, j.req.Path, j.json)
					if e != nil {
						mu.Lock()
						report.Differences = append(report.Differences, errorDifference(profile, j.req, e))
						fmt.Fprintf(os.Stderr, "ERROR %s %s: %v\n", profile, j.req.Path, e)
						mu.Unlock()
						continue
					}
					fixtureMu.Lock()
					n, e := pmsconform.SaveFixtureTrimmed(filepath.Join(out, profile), filename, resp)
					fixtureMu.Unlock()
					mu.Lock()
					if e != nil {
						report.Differences = append(report.Differences, errorDifference(profile, j.req, e))
						fmt.Fprintf(os.Stderr, "ERROR saving %s %s: %v\n", profile, j.req.Path, e)
					} else if n > 0 {
						report.Discovery = append(report.Discovery, fmt.Sprintf("%s %s trimmed to first 50 entries", profile, j.req.Path))
					}
					mu.Unlock()
				}
			}()
		}
		for _, req := range corpus(only) {
			for _, jm := range []bool{false, true} {
				jobs <- job{req, jm}
			}
		}
		close(jobs)
		wg.Wait()
	}
	if e := writeManifest(out, report, only); e != nil {
		return report, e
	}
	return report, nil
}
func compare(dir, base, token, sel, only string, timeout time.Duration, concurrency int) (pmsconform.Report, error) {
	client := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	out := pmsconform.Report{}
	for _, req := range corpus(only) {
		out.Endpoints = append(out.Endpoints, req.Path)
	}
	for _, profile := range profiles(sel) {
		refIDs, e := loadDiscovery(dir, profile)
		if e != nil {
			refIDs = pmsconform.DiscoveredIDs{Values: map[string]string{}, Warnings: []string{"reference discovery unavailable; fallback IDs may be used"}}
		}
		targetIDs := conformplex.Discover(ctx, client, base, token, profile, &refIDs)
		for _, w := range refIDs.Warnings {
			out.Discovery = append(out.Discovery, profile+" reference: "+w)
		}
		for _, w := range targetIDs.Warnings {
			out.Discovery = append(out.Discovery, profile+" target: "+w)
		}
		type job struct {
			req  pmsconform.Request
			json bool
		}
		type result struct{ diff []pmsconform.Difference }
		jobs := make(chan job)
		results := make(chan result)
		var wg sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					name := fixtureName(profile, j.req.Path, j.json)
					a, e := pmsconform.LoadFixture(filepath.Join(dir, profile, name+".json"))
					if e != nil {
						d := errorDifference(profile, j.req, fmt.Errorf("load fixture: %w", e))
						d.Format = formatName(j.json)
						fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", d.Path, d.Reference)
						results <- result{[]pmsconform.Difference{d}}
						continue
					}
					b, e := conformplex.Fetch(ctx, client, base, token, j.req, profile, targetIDs.Values, j.json)
					if e != nil {
						d := errorDifference(profile, j.req, e)
						d.Format = formatName(j.json)
						fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", d.Path, d.Reference)
						results <- result{[]pmsconform.Difference{d}}
						continue
					}
					r := pmsconform.Compare(a, b).WithRequest(profile, formatName(j.json), j.req.Path)
					results <- result{r.Differences}
				}
			}()
		}
		go func() {
			for _, req := range corpus(only) {
				for _, jm := range []bool{false, true} {
					jobs <- job{req, jm}
				}
			}
			close(jobs)
			wg.Wait()
			close(results)
		}()
		for r := range results {
			out.Differences = append(out.Differences, r.diff...)
		}
	}
	return out, nil
}
func fixtureName(profile, path string, jsonMode bool) string {
	return pmsconform.FixtureName(profile, path, jsonMode)
}
func formatName(jsonMode bool) string {
	if jsonMode {
		return "json"
	}
	return "xml"
}
func errorDifference(profile string, req pmsconform.Request, e error) pmsconform.Difference {
	return pmsconform.Difference{Client: profile, Endpoint: req.Path, Path: profile + " " + req.Method + " " + req.Path, Kind: pmsconform.KindRequestError, Reference: string(pmsconform.Redact([]byte(e.Error()))), Blocking: true}
}
func mergeReports(a, b pmsconform.Report) pmsconform.Report {
	a.Differences = append(a.Differences, b.Differences...)
	a.Discovery = append(a.Discovery, b.Discovery...)
	return a
}
func emit(w io.Writer, r pmsconform.Report, format string, summary, values bool) {
	if format == "json" {
		var v any = r
		if summary {
			v = struct {
				Discovery []string                     `json:"discovery,omitempty"`
				Endpoints []pmsconform.EndpointSummary `json:"endpoints"`
			}{r.Discovery, pmsconform.Summarize(r)}
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintln(w, string(b))
		return
	}
	if summary {
		pmsconform.WriteSummary(w, r, values)
		return
	}
	pmsconform.WriteText(w, r, values)
}
func writeManifest(dir string, r pmsconform.Report, only string) error {
	if e := os.MkdirAll(dir, 0755); e != nil {
		return e
	}
	trimmed, discovery := []string{}, []string{}
	if only != "" {
		// Keep what the previous recording noted about the other requests.
		var old struct {
			Trimmed   []string `json:"trimmedResponses"`
			Discovery []string `json:"discoveryWarnings"`
		}
		if b, e := os.ReadFile(filepath.Join(dir, "manifest.json")); e == nil && json.Unmarshal(b, &old) == nil {
			for _, line := range old.Trimmed {
				if !strings.Contains(line, only) {
					trimmed = append(trimmed, line)
				}
			}
			discovery = append(discovery, old.Discovery...)
		}
	}
	for _, line := range r.Discovery {
		if strings.Contains(line, "trimmed to first 50") {
			trimmed = append(trimmed, line)
		} else {
			discovery = append(discovery, line)
		}
	}
	counts := map[string]int{}
	for _, profile := range []string{"web", "ios", "appletv"} {
		entries, _ := os.ReadDir(filepath.Join(dir, profile))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") && !strings.HasPrefix(entry.Name(), "_") {
				counts[profile]++
			}
		}
	}
	data := map[string]any{"fixturePolicy": "Response listings are trimmed to the first 50 entries per repeated container child.", "trimmedResponses": trimmed, "fixtureCounts": counts, "discoveryWarnings": discovery, "requestErrors": r.Differences}
	b, e := json.MarshalIndent(data, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0644)
}
func saveDiscovery(dir, profile string, ids pmsconform.DiscoveredIDs) error {
	if e := os.MkdirAll(filepath.Join(dir, profile), 0755); e != nil {
		return e
	}
	b, e := json.MarshalIndent(ids, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join(dir, profile, "_discovery.json"), b, 0644)
}
func loadDiscovery(dir, profile string) (pmsconform.DiscoveredIDs, error) {
	b, e := os.ReadFile(filepath.Join(dir, profile, "_discovery.json"))
	if e != nil {
		return pmsconform.DiscoveredIDs{}, e
	}
	var d pmsconform.DiscoveredIDs
	e = json.Unmarshal(b, &d)
	return d, e
}
