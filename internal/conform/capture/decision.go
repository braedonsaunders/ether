package capture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
)

// plexClientHeaders is the order official Plex apps send identity headers.
var plexClientHeaders = []string{
	"Accept",
	"User-Agent",
	"X-Plex-Product",
	"X-Plex-Version",
	"X-Plex-Client-Identifier",
	"X-Plex-Platform",
	"X-Plex-Platform-Version",
	"X-Plex-Device",
	"X-Plex-Device-Name",
	"X-Plex-Device-Screen-Resolution",
	"X-Plex-Language",
	"X-Plex-Features",
	"X-Plex-Model",
	"X-Plex-Provides",
	"X-Plex-Client-Profile-Name",
	"X-Plex-Client-Profile-Extra",
	"X-Plex-Client-Capabilities",
}

// DecisionsFromCapture writes a redacted Plex decision corpus from a proxy capture.
func DecisionsFromCapture(dir, client, out string) error {
	if !slug.MatchString(client) {
		return fmt.Errorf("invalid client name")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no capture files")
	}
	sort.Strings(files)
	corpus := conformplex.DecisionCorpus{
		Version: 1,
		Policy:  "Redacted official-client decision requests captured through conformdiff proxy. Query pairs keep wire order. Tokens and hosts stripped. Do not point the proxy at HYPERV03 or claim a Plex server on the owner account.",
	}
	seen := map[string]bool{}
	for i, file := range files {
		b, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		var entry Entry
		if e = json.Unmarshal(b, &entry); e != nil {
			return e
		}
		if entry.Sequence != i+1 {
			return fmt.Errorf("invalid capture sequence: %s", file)
		}
		if entry.Protocol != "" && entry.Protocol != "plex" {
			return fmt.Errorf("capture protocol %q is not plex", entry.Protocol)
		}
		if entry.ResponseOrigin == "proxy-error" {
			continue
		}
		if entry.ResponseOrigin != "reference" {
			return fmt.Errorf("sequence %d has ambiguous response origin", entry.Sequence)
		}
		path, raw, _ := strings.Cut(entry.Request.Path, "?")
		if path != conformplex.DecisionPath {
			continue
		}
		format := "xml"
		if strings.Contains(strings.ToLower(entry.Request.Headers.Get("Accept")), "json") {
			format = "json"
		}
		query := conformplex.QueryPairs(raw)
		rec := conformplex.DecisionRecord{
			ID:     fmt.Sprintf("%s-%s-%06d", client, format, entry.Sequence),
			Client: client,
			Format: format,
			Source: "conformdiff proxy capture",
			Request: conformplex.DecisionRequest{
				Method:  entry.Request.Method,
				Path:    path,
				Query:   query,
				Headers: orderedPlexHeaders(entry.Request.Headers, query),
				Raw:     entry.Request.Path,
			},
		}
		key := rec.Request.Raw + "|" + format
		if seen[key] {
			continue
		}
		seen[key] = true
		corpus.Records = append(corpus.Records, rec)
	}
	if len(corpus.Records) == 0 {
		return fmt.Errorf("no decision requests in capture")
	}
	if out == "" {
		out = filepath.Join("testdata", "conform", "plex", "decisions", "corpus.json")
	}
	if err = os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(b, '\n'), 0644)
}

func orderedPlexHeaders(h http.Header, query [][2]string) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	for _, k := range plexClientHeaders {
		if v := h.Get(k); v != "" {
			out = append(out, [2]string{k, v})
			seen[http.CanonicalHeaderKey(k)] = true
		}
	}
	var extra []string
	for k := range h {
		if seen[http.CanonicalHeaderKey(k)] || !strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Plex-") {
			continue
		}
		if strings.EqualFold(k, "X-Plex-Token") {
			out = append(out, [2]string{"X-Plex-Token", "[REDACTED]"})
			continue
		}
		extra = append(extra, k)
	}
	sort.Strings(extra)
	for _, k := range extra {
		out = append(out, [2]string{k, h.Get(k)})
	}
	if !seen[http.CanonicalHeaderKey("X-Plex-Client-Profile-Extra")] {
		value := h.Get("X-Plex-Client-Profile-Extra")
		if value == "" {
			for _, q := range query {
				if q[0] == "X-Plex-Client-Profile-Extra" {
					value = q[1]
					break
				}
			}
		}
		out = append(out, [2]string{"X-Plex-Client-Profile-Extra", value})
	}
	return out
}
