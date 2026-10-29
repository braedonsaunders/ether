package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
)

// profileScenario maps a recorded, read-only probe to two explicitly aligned targets.
func profileScenario(s scenario) (scenario, error) {
	data, e := os.ReadFile(s.ProfileFile)
	if e != nil {
		return scenario{}, fmt.Errorf("cannot read profile")
	}
	var p mediabrowser.Profile
	if e = json.Unmarshal(data, &p); e != nil || p.Client == "" {
		return scenario{}, fmt.Errorf("invalid profile")
	}
	var selected *mediabrowser.Probe
	for _, probe := range p.Flows[s.Flow] {
		if probe.Name == s.Probe {
			if selected != nil {
				return scenario{}, fmt.Errorf("ambiguous profile probe")
			}
			copy := probe
			selected = &copy
		}
	}
	if selected == nil || strings.ToUpper(selected.Method) != "GET" || selected.FixtureOnly || selected.Binary || selected.Body != nil || selected.BodyBase64 != "" {
		return scenario{}, fmt.Errorf("profile probe must be a non-fixture text GET")
	}
	if !strings.HasPrefix(selected.Path, "/") || strings.HasPrefix(selected.Path, "//") {
		return scenario{}, fmt.Errorf("invalid profile path")
	}
	for _, dst := range []*target{&s.Reference, &s.Ether} {
		if dst.URL == "" {
			return scenario{}, fmt.Errorf("profile target URL is required")
		}
		path, e := expandProfilePath(selected.Path, dst.Variables)
		if e != nil {
			return scenario{}, e
		}
		dst.URL = strings.TrimRight(dst.URL, "/") + path
		headers := map[string]string{}
		for k, v := range p.Headers {
			value, e := expandProfileHeader(v, dst.Variables)
			if e != nil {
				return scenario{}, e
			}
			headers[http.CanonicalHeaderKey(k)] = value
		}
		for k, v := range selected.Headers {
			value, e := expandProfileHeader(v, dst.Variables)
			if e != nil {
				return scenario{}, e
			}
			headers[http.CanonicalHeaderKey(k)] = value
		}
		for k, v := range dst.Headers {
			value, e := expandProfileHeader(v, dst.Variables)
			if e != nil {
				return scenario{}, e
			}
			headers[http.CanonicalHeaderKey(k)] = value
		}
		dst.Headers = headers
	}
	return s, nil
}
func expandProfileHeader(value string, vars map[string]string) (string, error) {
	expression := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	missing := false
	out := expression.ReplaceAllStringFunc(value, func(token string) string {
		v, ok := vars[token[2:len(token)-1]]
		if !ok {
			missing = true
		}
		return v
	})
	if missing || strings.Contains(out, "${") {
		return "", fmt.Errorf("missing profile variable")
	}
	return out, nil
}

func expandProfilePath(value string, vars map[string]string) (string, error) {
	expression := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	queryAt := strings.IndexByte(value, '?')
	var out strings.Builder
	last := 0
	for _, index := range expression.FindAllStringSubmatchIndex(value, -1) {
		out.WriteString(value[last:index[0]])
		key := value[index[2]:index[3]]
		v, ok := vars[key]
		if !ok {
			return "", fmt.Errorf("missing profile path variable")
		}
		if queryAt >= 0 && index[0] > queryAt {
			out.WriteString(url.QueryEscape(v))
		} else {
			out.WriteString(url.PathEscape(v))
		}
		last = index[1]
	}
	out.WriteString(value[last:])
	if strings.Contains(out.String(), "${") {
		return "", fmt.Errorf("invalid profile path variable")
	}
	return out.String(), nil
}
