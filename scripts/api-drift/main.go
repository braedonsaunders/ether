// api-drift checks pinned protocol descriptions against upstream releases.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/conform"
)

type spec struct {
	Protocol, Version, URL, SHA256, Format string
	LatestURL                              string `json:"latest_url"`
	RouteURL                               string `json:"route_inventory_url"`
	RouteSHA                               string `json:"route_inventory_sha256"`
}
type change struct{ Protocol, Kind, Method, Path, Priority string }

var client = &http.Client{Timeout: 45 * time.Second}

func fetch(u string) ([]byte, error) {
	r, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", u, r.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, (64<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<20 {
		return nil, fmt.Errorf("%s exceeds size limit", u)
	}
	return b, nil
}

var jellyfinRelease = regexp.MustCompile(`jellyfin-openapi-([0-9]+(?:\.[0-9]+)+)\.json`)

func latestSpec(s spec) ([]byte, error) {
	if s.Protocol != "jellyfin" || !strings.HasSuffix(s.LatestURL, "/") {
		return fetch(s.LatestURL)
	}
	index, err := fetch(s.LatestURL)
	if err != nil {
		return nil, err
	}
	var best []int
	var file string
	for _, match := range jellyfinRelease.FindAllStringSubmatch(string(index), -1) {
		var version []int
		for _, part := range strings.Split(match[1], ".") {
			n, e := strconv.Atoi(part)
			if e != nil {
				return nil, e
			}
			version = append(version, n)
		}
		if file == "" || versionGreater(version, best) {
			best = version
			file = match[0]
		}
	}
	if file == "" {
		return nil, fmt.Errorf("no stable Jellyfin release found at %s", s.LatestURL)
	}
	return fetch(s.LatestURL + file)
}
func versionGreater(a, b []int) bool {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}
func verify(b []byte, want string) error {
	sum := sha256.Sum256(b)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
		return fmt.Errorf("pinned spec checksum mismatch")
	}
	return nil
}
func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func operations(b []byte) (map[string]string, error) {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, err
	}
	paths, ok := root["paths"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("OpenAPI paths missing")
	}
	out := map[string]string{}
	for path, val := range paths {
		methods, _ := val.(map[string]any)
		for method, op := range methods {
			switch strings.ToUpper(method) {
			case "GET", "PUT", "POST", "DELETE", "PATCH", "OPTIONS", "HEAD", "TRACE":
				refs := map[string]any{}
				collectRefs(root, op, refs)
				out[strings.ToUpper(method)+" "+path] = fingerprint(map[string]any{"operation": op, "references": refs})
			}
		}
	}
	return out, nil
}

// collectRefs includes referenced component definitions so changing a schema
// also marks operations that use it as changed. Recursive schemas terminate via seen.
func collectRefs(root map[string]any, v any, seen map[string]any) {
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			if _, ok := seen[ref]; !ok {
				var target any = root
				for _, part := range strings.Split(ref[2:], "/") {
					part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
					m, _ := target.(map[string]any)
					target = m[part]
				}
				seen[ref] = target
				if target != nil {
					collectRefs(root, target, seen)
				}
			}
		}
		for _, child := range x {
			collectRefs(root, child, seen)
		}
	case []any:
		for _, child := range x {
			collectRefs(root, child, seen)
		}
	}
}
func inventory(b []byte) (map[string]string, error) {
	var data struct {
		Routes []json.RawMessage `json:"routes"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data.Routes == nil {
		return nil, fmt.Errorf("route inventory missing routes")
	}
	out := map[string]string{}
	for _, raw := range data.Routes {
		var r struct{ Method, Path string }
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if r.Method == "" || r.Path == "" {
			return nil, fmt.Errorf("route missing method or path")
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out[strings.ToUpper(r.Method)+" "+r.Path] = fingerprint(v)
	}
	return out, nil
}

var variable = regexp.MustCompile(`\$?\{[^}]+\}`)

func shape(path string) string {
	u, err := url.Parse(path)
	if err == nil {
		path = u.Path
	}
	return variable.ReplaceAllString(path, "{}")
}
func key(method, path string) string { return strings.ToUpper(method) + " " + shape(path) }
func profileRoutes(protocol string) (map[string]bool, error) {
	files, err := filepath.Glob(filepath.Join("internal", "conform", "profiles", protocol, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no %s profiles", protocol)
	}
	out := map[string]bool{}
	for _, f := range files {
		b, e := os.ReadFile(f)
		if e != nil {
			return nil, e
		}
		var v struct {
			Flows map[string][]struct{ Method, Path string } `json:"flows"`
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		for _, requests := range v.Flows {
			for _, r := range requests {
				out[key(r.Method, r.Path)] = true
			}
		}
	}
	return out, nil
}
func registryRoutes(bin, protocol string) (map[string]bool, bool, error) {
	b, err := exec.Command(bin, "routes", "-protocol", protocol).Output()
	if err != nil {
		return nil, false, fmt.Errorf("route inventory %s: %w", protocol, err)
	}
	var data conform.RouteInventory
	if err = json.Unmarshal(b, &data); err != nil {
		return nil, false, err
	}
	out := map[string]bool{}
	for _, r := range data.Routes {
		out[key(r.Method, r.Pattern)] = true
	}
	return out, data.Available, nil
}
func diff(protocol string, old, new map[string]string, used, implemented map[string]bool) []change {
	var result []change
	for k, oldVal := range old {
		newVal, ok := new[k]
		if !ok {
			result = append(result, makeChange(protocol, "removed", k, used, implemented))
		} else if oldVal != newVal {
			result = append(result, makeChange(protocol, "changed", k, used, implemented))
		}
	}
	for k := range new {
		if _, ok := old[k]; !ok {
			result = append(result, makeChange(protocol, "added", k, used, implemented))
		}
	}
	return result
}
func makeChange(protocol, kind, k string, used, implemented map[string]bool) change {
	parts := strings.SplitN(k, " ", 2)
	priority := "other"
	if implemented[key(parts[0], parts[1])] {
		priority = "implemented"
	}
	if used[key(parts[0], parts[1])] {
		priority = "used-by-clients"
	}
	return change{protocol, kind, parts[0], parts[1], priority}
}

// openDefects reports locally recorded reference issues without claiming to
// recheck server behavior; that requires the reference lab.
func openDefects(protocol string) ([]string, error) {
	path := filepath.Join("testdata", "conform", protocol, "reference-defects.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Defects []struct{ ID, Detail, Observed, Status string } `json:"defects"`
	}
	if err = json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var notes []string
	for _, d := range doc.Defects {
		if d.Status == "open" {
			if d.ID == "" {
				return nil, fmt.Errorf("reference defect has no id")
			}
			notes = append(notes, fmt.Sprintf("%s reference defect %s (%s): %s; lab recheck required", protocol, d.ID, d.Observed, d.Detail))
		}
	}
	return notes, nil
}
func run() error {
	manifest := flag.String("manifest", "internal/conform/specs/manifest.json", "spec manifest")
	bin := flag.String("conformdiff", "conformdiff", "conformdiff executable")
	out := flag.String("out", "api-drift-report.md", "report output")
	flag.Parse()
	b, err := os.ReadFile(*manifest)
	if err != nil {
		return err
	}
	var doc struct {
		Specs []spec `json:"specs"`
	}
	if err = json.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.Specs) == 0 {
		return fmt.Errorf("empty manifest")
	}
	var changes []change
	var notes []string
	for _, s := range doc.Specs {
		defects, err := openDefects(s.Protocol)
		if err != nil {
			return err
		}
		notes = append(notes, defects...)
		old, err := fetch(s.URL)
		if err != nil {
			return err
		}
		if err = verify(old, s.SHA256); err != nil {
			return fmt.Errorf("%s: %w", s.Protocol, err)
		}
		latest, err := latestSpec(s)
		if err != nil {
			return err
		}
		used, err := profileRoutes(s.Protocol)
		if err != nil {
			return err
		}
		implemented, available, err := registryRoutes(*bin, s.Protocol)
		if err != nil {
			return err
		}
		if !available {
			notes = append(notes, s.Protocol+": front-end route inventory unavailable; all upstream changes shown")
		}
		a, err := operations(old)
		if err != nil {
			return fmt.Errorf("%s pinned: %w", s.Protocol, err)
		}
		z, err := operations(latest)
		if err != nil {
			return fmt.Errorf("%s latest: %w", s.Protocol, err)
		}
		changes = append(changes, diff(s.Protocol, a, z, used, implemented)...)
		if s.Protocol == "silo" && s.RouteURL != "" {
			oldRoutes, err := fetch(s.RouteURL)
			if err != nil {
				return err
			}
			if err = verify(oldRoutes, s.RouteSHA); err != nil {
				return err
			}
			latestURL := strings.Replace(s.LatestURL, "/openapi.json", "/route-inventory.json", 1)
			if latestURL == s.LatestURL {
				return fmt.Errorf("cannot derive Silo latest route inventory URL")
			}
			latestRoutes, err := fetch(latestURL)
			if err != nil {
				return err
			}
			a, err = inventory(oldRoutes)
			if err != nil {
				return err
			}
			z, err = inventory(latestRoutes)
			if err != nil {
				return err
			}
			changes = append(changes, diff("silo-routes", a, z, used, implemented)...)
		}
	}
	if len(changes) == 0 {
		fmt.Println("No endpoint drift detected.")
		var defects []string
		for _, n := range notes {
			if strings.Contains(n, " reference defect ") {
				defects = append(defects, n)
			}
		}
		if len(defects) == 0 {
			return os.WriteFile(*out, nil, 0644)
		}
		report := "# Open reference defects\n\n"
		for _, n := range defects {
			report += "- " + n + "\n"
		}
		return os.WriteFile(*out, []byte(report), 0644)
	}
	rank := map[string]int{"used-by-clients": 0, "implemented": 1, "other": 2}
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if rank[a.Priority] != rank[b.Priority] {
			return rank[a.Priority] < rank[b.Priority]
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Method < b.Method
	})
	var report strings.Builder
	report.WriteString("# API endpoint drift\n\n")
	for _, note := range notes {
		fmt.Fprintf(&report, "- %s\n", note)
	}
	report.WriteString("\n| Priority | Protocol | Change | Method | Endpoint |\n|---|---|---|---|---|\n")
	for _, c := range changes {
		fmt.Fprintf(&report, "| %s | %s | %s | %s | `%s` |\n", c.Priority, c.Protocol, c.Kind, c.Method, strings.ReplaceAll(c.Path, "|", "\\|"))
	}
	return os.WriteFile(*out, []byte(report.String()), 0644)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "API drift check failed:", err)
		os.Exit(1)
	}
}
