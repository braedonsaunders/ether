// conformdiff records and compares protocol responses and checks offline reference drift.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	pmsconform "github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/conform/silo"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: conformdiff record|compare|live|drift [flags]")
	}
	cmd := args[0]
	if cmd == "drift" {
		return drift(args[1:])
	}
	if cmd == "proxy" {
		return proxyCommand(args[1:])
	}
	if cmd == "fixtures-from-capture" {
		return fixturesFromCapture(args[1:])
	}
	if cmd == "profile-from-capture" {
		return profileFromCapture(args[1:])
	}
	if cmd == "routes" {
		return routes(args[1:])
	}
	if cmd == "validate" {
		return validateFixture(args[1:])
	}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	ref := f.String("reference", "", "reference server URL")
	fixtures := f.String("fixtures", "", "recorded reference fixture directory")
	protocol := f.String("protocol", "plex", "plex|jellyfin|emby|silo")
	profileDir := f.String("profiles", "", "profile directory")
	refToken := f.String("reference-token", os.Getenv("CONFORM_REFERENCE_TOKEN"), "reference credential")
	targetToken := f.String("target-token", os.Getenv("CONFORM_TARGET_TOKEN"), "target credential")
	refVars := f.String("reference-vars", "", "JSON file containing reference template variables")
	targetVars := f.String("target-vars", "", "JSON file containing target template variables")
	refServer := f.String("reference-server", "", "live reference PMS URL")
	target := f.String("target", "", "target PMS URL")
	server := f.String("server", "", "PMS URL to record")
	token := f.String("token", "", "PMS access token")
	out := f.String("out", "", "output fixture directory")
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
	var p pmsconform.Protocol
	if *protocol == "plex" {
		p = conformplex.Protocol{}
	} else {
		dir := *profileDir
		if dir == "" {
			dir = filepath.Join("internal", "conform", "profiles", *protocol)
		}
		var e error
		if *protocol == "silo" {
			p, e = silo.Load(dir)
		} else {
			p, e = mediabrowser.Load(*protocol, dir)
		}
		if e != nil {
			return e
		}
	}
	if *client != "all" && *client != "" {
		for _, name := range strings.Split(*client, ",") {
			if _, ok := p.Clients()[name]; !ok {
				return fmt.Errorf("unknown client %q", name)
			}
		}
	}
	if *protocol != "plex" {
		*concurrency = 1
	}
	if *out == "" {
		*out = filepath.Join("testdata", "conform", *protocol)
	}
	if *fixtures == "" {
		*fixtures = *out
	}
	if *refToken == "" {
		*refToken = *token
	}
	if *targetToken == "" {
		*targetToken = *token
	}
	referenceVars, err := readVariables(*refVars)
	if err != nil {
		return err
	}
	targetVariables, err := readVariables(*targetVars)
	if err != nil {
		return err
	}
	if mb, ok := p.(*mediabrowser.Protocol); ok && *protocol == "jellyfin" && (cmd == "compare" || cmd == "live") {
		for _, profile := range profiles(p, *client) {
			if err := mb.LoadCapturedBindings(*fixtures, profile); err != nil {
				return err
			}
		}
	}
	var report pmsconform.Report
	switch cmd {
	case "defects":
		if *protocol != "emby" || *ref == "" {
			return fmt.Errorf("defects requires -protocol emby and -reference URL")
		}
		return checkDefects(p, referenceVars, *ref, *refToken, *client, *timeout, *concurrency)
	case "record":
		if *server == "" {
			*server = *ref
		}
		if *server == "" {
			return fmt.Errorf("record requires --server")
		}
		report, err = record(p, referenceVars, *server, *refToken, *out, *client, *only, *timeout, *concurrency)
	case "compare":
		if *target == "" {
			return fmt.Errorf("compare requires --target")
		}
		report, err = compare(p, targetVariables, *fixtures, *target, *targetToken, *client, *only, *timeout, *concurrency)
	case "live":
		if *refServer == "" {
			*refServer = *ref
		}
		if *refServer == "" || *target == "" {
			return fmt.Errorf("live requires --reference-server and --target")
		}
		tmp, e := os.MkdirTemp("", "conformdiff-reference-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(tmp)
		report, err = record(p, referenceVars, *refServer, *refToken, tmp, *client, *only, *timeout, *concurrency)
		if err == nil {
			var targetReport pmsconform.Report
			targetReport, err = compare(p, targetVariables, tmp, *target, *targetToken, *client, *only, *timeout, *concurrency)
			if err == nil {
				// The temporary live recording has its own generated manifest;
				// reviewed field exceptions come from the checked-in corpus.
				var rules map[string]string
				rules, err = loadNotApplicable(*fixtures)
				if err == nil {
					for _, endpoint := range targetReport.Endpoints {
						applyNotApplicable(&targetReport, endpoint, rules)
					}
				}
			}
			report = mergeReports(report, targetReport)
		}
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		return err
	}
	ignorePatterns := append([]string(nil), p.IgnorePaths()...)
	if *ignore != "" {
		ignorePatterns = append(ignorePatterns, *ignore)
	}
	for _, pattern := range ignorePatterns {
		re, e := regexp.Compile(pattern)
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

func profiles(p pmsconform.Protocol, sel string) []string {
	if sel == "all" || sel == "" {
		names := make([]string, 0, len(p.Clients()))
		for name := range p.Clients() {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}
	return strings.Split(sel, ",")
}

// corpus is the probe corpus, optionally narrowed to paths containing only.
func corpus(p pmsconform.Protocol, profile, only string) []pmsconform.Request {
	all := p.Corpus(profile)
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

func record(p pmsconform.Protocol, variables map[string]string, base, token, out, sel, only string, timeout time.Duration, concurrency int) (pmsconform.Report, error) {
	if _, ok := p.(pmsconform.ResponseObserver); ok {
		concurrency = 1
	}
	client := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	report := pmsconform.Report{}
	var mu sync.Mutex
	for _, profile := range profiles(p, sel) {
		requests := corpus(p, profile, only)
		if len(requests) == 0 {
			return report, fmt.Errorf("no requests match client %q and filter %q", profile, only)
		}
		// A partial recording reuses the saved IDs so the new fixtures
		// describe the same items as the rest of the set.
		ids, e := loadDiscovery(out, profile)
		if only == "" || e != nil {
			ids = p.Discover(ctx, client, base, token, profile, withRunDevice(nil, variables))
			for _, w := range ids.Warnings {
				report.Discovery = append(report.Discovery, profile+": "+w)
			}
			if e := saveDiscovery(out, profile, ids); e != nil {
				return report, e
			}
		}
		for k, v := range variables {
			ids.Values[k] = v
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
					resp, e := p.Fetch(ctx, client, base, token, j.req, profile, ids.Values, j.json)
					filename := p.FixtureName(profile, j.req, j.json)
					if e != nil {
						mu.Lock()
						report.Differences = append(report.Differences, errorDifference(profile, j.req, e))
						fmt.Fprintf(os.Stderr, "ERROR %s %s: %v\n", profile, j.req.Path, e)
						mu.Unlock()
						continue
					}
					if observer, ok := p.(pmsconform.ResponseObserver); ok {
						observer.Observe(j.req, resp, ids.Values)
					}
					fixtureMu.Lock()
					n, e := pmsconform.SaveFixtureTrimmed(filepath.Join(out, profile), filename, scrubSecrets(p.Redact(resp), ids.Values, token))
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
		for _, req := range requests {
			if req.FixtureOnly {
				continue
			}
			for _, jm := range p.Formats() {
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
func compare(p pmsconform.Protocol, variables map[string]string, dir, base, token, sel, only string, timeout time.Duration, concurrency int) (pmsconform.Report, error) {
	notApplicable, err := loadNotApplicable(dir)
	if err != nil {
		return pmsconform.Report{}, err
	}
	if _, ok := p.(pmsconform.ResponseObserver); ok {
		concurrency = 1
	}
	client := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	out := pmsconform.Report{}
	for _, profile := range profiles(p, sel) {
		requests := corpus(p, profile, only)
		if len(requests) == 0 {
			return out, fmt.Errorf("no requests match client %q and filter %q", profile, only)
		}
		for _, req := range requests {
			found := false
			for _, endpoint := range out.Endpoints {
				if endpoint == req.Path {
					found = true
					break
				}
			}
			if !found {
				out.Endpoints = append(out.Endpoints, req.Path)
			}
		}
		refIDs, e := loadDiscovery(dir, profile)
		if e != nil {
			refIDs = pmsconform.DiscoveredIDs{Values: map[string]string{}, Warnings: []string{"reference discovery unavailable; fallback IDs may be used"}}
		}
		targetIDs := p.Discover(ctx, client, base, token, profile, withRunDevice(&refIDs, variables))
		for k, v := range variables {
			targetIDs.Values[k] = v
		}
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
		type result struct{ report pmsconform.Report }
		jobs := make(chan job)
		results := make(chan result)
		var wg sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					name := p.FixtureName(profile, j.req, j.json)
					a, e := pmsconform.LoadFixture(filepath.Join(dir, profile, name+".json"))
					if e != nil {
						d := errorDifference(profile, j.req, fmt.Errorf("load fixture: %w", e))
						d.Format = formatName(j.json)
						fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", d.Path, d.Reference)
						results <- result{pmsconform.Report{Differences: []pmsconform.Difference{d}}}
						continue
					}
					b, e := p.Fetch(ctx, client, base, token, j.req, profile, targetIDs.Values, j.json)
					if e != nil {
						d := errorDifference(profile, j.req, e)
						d.Format = formatName(j.json)
						fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", d.Path, d.Reference)
						results <- result{pmsconform.Report{Differences: []pmsconform.Difference{d}}}
						continue
					}
					if observer, ok := p.(pmsconform.ResponseObserver); ok {
						observer.Observe(j.req, b, targetIDs.Values)
					}
					r := compareRedacted(p, j.req, a, b, targetIDs.Values, token).WithRequest(profile, formatName(j.json), j.req.Path)
					applyNotApplicable(&r, j.req.Path, notApplicable)
					results <- result{r}
				}
			}()
		}
		go func() {
			for _, req := range requests {
				if req.FixtureOnly {
					continue
				}
				for _, jm := range p.Formats() {
					jobs <- job{req, jm}
				}
			}
			close(jobs)
			wg.Wait()
			close(results)
		}()
		for r := range results {
			out.Differences = append(out.Differences, r.report.Differences...)
			out.ReferenceDefects = append(out.ReferenceDefects, r.report.ReferenceDefects...)
		}
	}
	return out, nil
}

func applyNotApplicable(report *pmsconform.Report, endpoint string, rules map[string]string) {
	for index := range report.Differences {
		d := &report.Differences[index]
		if d.Kind != pmsconform.KindMissing || d.Endpoint != endpoint {
			continue
		}
		if reason, ok := rules[endpoint+"\x00"+d.Path]; ok {
			d.Blocking = false
			d.Reason = reason
		}
	}
}

// loadNotApplicable accepts only explicit reviewed endpoint/field pairs. A
// missing field without both a reason and a client audit remains blocking.
func loadNotApplicable(dir string) (map[string]string, error) {
	result := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	var manifest struct {
		NotApplicable []struct{ Endpoint, Path, Reason, ClientAudit string } `json:"notApplicable"`
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	for _, rule := range manifest.NotApplicable {
		if rule.Endpoint == "" || rule.Path == "" || rule.Reason == "" || rule.ClientAudit == "" {
			return nil, fmt.Errorf("incomplete not-applicable manifest entry")
		}
		key := rule.Endpoint + "\x00" + rule.Path
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate not-applicable manifest entry")
		}
		result[key] = rule.Reason
	}
	return result, nil
}

// Binary range validation needs the exact structural headers. Compare those
// before redaction, then scrub the resulting report before it can be emitted.
// A short run-specific identifier must not turn "video/mp2t" or a byte range
// into invalid syntax.
func compareRedacted(p pmsconform.Protocol, request pmsconform.Request, reference, target pmsconform.Response, vars map[string]string, token string) pmsconform.Report {
	clean := scrubSecrets(p.Redact(target), vars, token)
	playlist := bytes.HasPrefix(bytes.TrimSpace(target.Body), []byte("#EXTM3U"))
	if target.Kind == "binary-range" || playlist {
		if clean.Headers == nil {
			clean.Headers = http.Header{}
		}
		for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Cache-Control"} {
			clean.Headers.Del(name)
			for _, value := range target.Headers.Values(name) {
				clean.Headers.Add(name, value)
			}
		}
	}
	if playlist {
		clean.Body = target.Body
	}
	var report pmsconform.Report
	if contextual, ok := p.(pmsconform.RequestComparer); ok {
		report = contextual.CompareRequest(request, reference, clean)
	} else {
		report = p.Compare(reference, clean)
	}
	if target.Kind == "binary-range" || playlist {
		raw, err := json.Marshal(report)
		if err != nil {
			return pmsconform.Report{Differences: []pmsconform.Difference{{Path: "$report", Kind: pmsconform.KindRequestError, Blocking: true}}}
		}
		redacted := scrubSecrets(pmsconform.Response{Body: raw}, vars, token)
		if json.Unmarshal(redacted.Body, &report) != nil {
			return pmsconform.Report{Differences: []pmsconform.Difference{{Path: "$report", Kind: pmsconform.KindRequestError, Blocking: true}}}
		}
	}
	return report
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

// withRunDevice supplies this run's device claim to discovery. It does not copy
// item aliases into ownership. An empty device leaves align unchanged.
func withRunDevice(align *pmsconform.DiscoveredIDs, variables map[string]string) *pmsconform.DiscoveredIDs {
	deviceID := ""
	if variables != nil {
		deviceID = variables["device_id"]
	}
	if deviceID == "" {
		return align
	}
	values := map[string]string{}
	if align != nil {
		for k, v := range align.Values {
			values[k] = v
		}
	}
	values["device_id"] = deviceID
	return &pmsconform.DiscoveredIDs{Values: values}
}
func errorDifference(profile string, req pmsconform.Request, e error) pmsconform.Difference {
	return pmsconform.Difference{Client: profile, Endpoint: req.Path, Path: profile + " " + req.Method + " " + req.Path, Kind: pmsconform.KindRequestError, Reference: string(pmsconform.Redact([]byte(e.Error()))), Blocking: true}
}
func mergeReports(a, b pmsconform.Report) pmsconform.Report {
	a.Differences = append(a.Differences, b.Differences...)
	a.Discovery = append(a.Discovery, b.Discovery...)
	a.ReferenceDefects = append(a.ReferenceDefects, b.ReferenceDefects...)
	return a
}
func emit(w io.Writer, r pmsconform.Report, format string, summary, values bool) {
	if format == "json" {
		var v any = r
		if summary {
			v = struct {
				Discovery        []string                     `json:"discovery,omitempty"`
				ReferenceDefects []pmsconform.ReferenceDefect `json:"reference_defects,omitempty"`
				Endpoints        []pmsconform.EndpointSummary `json:"endpoints"`
			}{r.Discovery, r.ReferenceDefects, pmsconform.Summarize(r)}
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
	profileDirs, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, d := range profileDirs {
		if !d.IsDir() {
			continue
		}
		profile := d.Name()
		entries, err := os.ReadDir(filepath.Join(dir, profile))
		if err != nil {
			return err
		}
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

// readVariables keeps supplied template values out of the saved discovery file.
func readVariables(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v map[string]string
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("variables: %w", err)
	}
	return v, nil
}

func scrubSecrets(r pmsconform.Response, variables map[string]string, token string) pmsconform.Response {
	secrets := []string{token}
	for _, key := range []string{"username", "password", "access_token", "server_id", "device_id", "user_id", "profile_id", "installation_id", "play_session_id", "session_id", "stop_id", "playback_attempt_id", "playback_attempt_id_direct", "playback_attempt_id_transcode", "stream_url", "segment_url", "stream_token"} {
		secrets = append(secrets, variables[key])
	}
	return pmsconform.RedactSecrets(r, secrets...)
}
