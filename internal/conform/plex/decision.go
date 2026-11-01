package plex

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
)

// DecisionCorpus is the offline oracle of official-client decision requests.
// Query pairs keep wire order. Header pairs keep the recorded client order.
// Tokens and hosts are redacted before check-in.
type DecisionCorpus struct {
	Version int              `json:"version"`
	Policy  string           `json:"policy"`
	Records []DecisionRecord `json:"records"`
}

// DecisionRecord is one redacted decision request plus the fixture it asserts.
type DecisionRecord struct {
	ID      string          `json:"id"`
	Client  string          `json:"client"`
	Format  string          `json:"format"`
	Source  string          `json:"source"`
	Request DecisionRequest `json:"request"`
}

// DecisionRequest is the exact decision the client issued.
type DecisionRequest struct {
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Query   [][2]string `json:"query"`
	Headers [][2]string `json:"headers"`
	// Raw is the path+query as recorded on the wire (or the browsing
	// corpus template). Fixture names and Ether replay use this so
	// encoding of {{tokens}} stays byte-identical to the recording.
	Raw string `json:"raw,omitempty"`
}

// DecisionPath is the Plex media-decision endpoint official clients call.
const DecisionPath = "/video/:/transcode/universal/decision"

// LoadDecisionCorpus reads a redacted decision corpus.
func LoadDecisionCorpus(path string) (DecisionCorpus, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return DecisionCorpus{}, err
	}
	var c DecisionCorpus
	if err = json.Unmarshal(b, &c); err != nil {
		return DecisionCorpus{}, err
	}
	if c.Version != 1 {
		return DecisionCorpus{}, fmt.Errorf("unsupported decision corpus version %d", c.Version)
	}
	if len(c.Records) == 0 {
		return DecisionCorpus{}, fmt.Errorf("empty decision corpus")
	}
	for i, rec := range c.Records {
		if rec.ID == "" || rec.Client == "" || rec.Request.Path != DecisionPath {
			return DecisionCorpus{}, fmt.Errorf("record %d is not a decision", i)
		}
		if rec.Format != "xml" && rec.Format != "json" {
			return DecisionCorpus{}, fmt.Errorf("record %s has format %q", rec.ID, rec.Format)
		}
		if rec.Request.Method == "" {
			c.Records[i].Request.Method = "GET"
		}
	}
	return c, nil
}

// Target is the path+query to replay. Raw wins so fixture names stay stable.
func (r DecisionRequest) Target() string {
	if r.Raw != "" {
		return r.Raw
	}
	if len(r.Query) == 0 {
		return r.Path
	}
	var b strings.Builder
	b.WriteString(r.Path)
	b.WriteByte('?')
	for i, pair := range r.Query {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(pair[0]))
		b.WriteByte('=')
		b.WriteString(strings.ReplaceAll(url.QueryEscape(pair[1]), "+", "%20"))
	}
	return b.String()
}

// HeaderMap is the request header set, last value wins for a name.
func (r DecisionRequest) HeaderMap() map[string]string {
	out := map[string]string{}
	for _, pair := range r.Headers {
		if !strings.EqualFold(pair[0], "X-Plex-Token") {
			out[pair[0]] = pair[1]
		}
	}
	return out
}

// FixturePath is the recorded PMS response for this decision.
func (r DecisionRecord) FixturePath(root string) string {
	jsonMode := r.Format == "json"
	return filepath.Join(root, r.Client, conform.FixtureName(r.Client, r.Request.Target(), jsonMode)+".json")
}

// QueryPairs parses a raw query string without reordering.
func QueryPairs(raw string) [][2]string {
	if raw == "" {
		return nil
	}
	var out [][2]string
	for raw != "" {
		var pair string
		pair, raw, _ = strings.Cut(raw, "&")
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		k, _ = url.QueryUnescape(k)
		v, _ = url.QueryUnescape(v)
		out = append(out, [2]string{k, v})
	}
	return out
}

// SeedDecisionCorpus builds the first corpus from the browsing decision
// probes. Official-client captures replace these headers/query extras.
func SeedDecisionCorpus() DecisionCorpus {
	clients := Clients()
	var records []DecisionRecord
	for _, req := range Corpus() {
		if !strings.HasPrefix(req.Path, DecisionPath) {
			continue
		}
		path, raw, _ := strings.Cut(req.Path, "?")
		query := QueryPairs(raw)
		for _, profile := range []string{"web", "ios", "appletv"} {
			c := clients[profile]
			for _, format := range []string{"xml", "json"} {
				extra := ""
				for _, q := range query {
					if q[0] == "X-Plex-Client-Profile-Extra" {
						extra = q[1]
						break
					}
				}
				headers := [][2]string{
					{"X-Plex-Product", c.Headers.Get("X-Plex-Product")},
					{"X-Plex-Version", c.Headers.Get("X-Plex-Version")},
					{"X-Plex-Platform", c.Headers.Get("X-Plex-Platform")},
					{"X-Plex-Platform-Version", c.Headers.Get("X-Plex-Platform-Version")},
					{"X-Plex-Device", c.Headers.Get("X-Plex-Device")},
					{"X-Plex-Device-Name", c.Headers.Get("X-Plex-Device-Name")},
					{"X-Plex-Client-Identifier", c.Headers.Get("X-Plex-Client-Identifier")},
					{"X-Plex-Provides", c.Headers.Get("X-Plex-Provides")},
					{"X-Plex-Client-Profile-Extra", extra},
				}
				if format == "json" {
					headers = append([][2]string{{"Accept", "application/json"}}, headers...)
				} else {
					headers = append([][2]string{{"Accept", "application/xml"}}, headers...)
				}
				id := profile + "-" + format + "-" + decisionID(query)
				records = append(records, DecisionRecord{
					ID:     id,
					Client: profile,
					Format: format,
					Source: "recorded PMS 1.43.4 browsing corpus; official-client header/query extras not yet captured",
					Request: DecisionRequest{
						Method:  "GET",
						Path:    path,
						Query:   query,
						Headers: headers,
						Raw:     req.Path,
					},
				})
			}
		}
	}
	return DecisionCorpus{
		Version: 1,
		Policy:  "Redacted official-client decision requests. Query pairs keep wire order. Tokens and hosts stripped. First seed is the recorded PMS 1.43.4 browsing corpus (representative client headers). A lab recording through conformdiff proxy against a disposable PMS — not the owner's production server, not the owner Plex account — is required to replace the seed with exact client Profile-Extra strings.",
		Records: records,
	}
}

func decisionID(query [][2]string) string {
	hasMDE := "legacy"
	direct := "direct"
	for _, p := range query {
		switch p[0] {
		case "hasMDE":
			if p[1] == "1" {
				hasMDE = "mde"
			}
		case "directPlay":
			if p[1] == "0" {
				direct = "stream"
			}
		}
	}
	if hasMDE == "legacy" {
		return "legacy"
	}
	return hasMDE + "-" + direct
}
