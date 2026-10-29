package conform

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Response is a captured HTTP response.
type Response struct {
	Status     int         `json:"status"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
	Kind       string      `json:"kind,omitempty"`
	RangeKind  string      `json:"rangeKind,omitempty"`
	BodyLength int64       `json:"bodyLength,omitempty"`
	SHA256     string      `json:"sha256,omitempty"`
}

// DiscoveredIDs holds per-server identifiers used to make templated probes target real items.
type DiscoveredIDs struct {
	Values   map[string]string `json:"values"`
	Warnings []string          `json:"warnings,omitempty"`
}

// Redact removes common account and host identifiers from captured XML/JSON/text.
func Redact(body []byte) []byte {
	s := string(body)
	patterns := []struct {
		re   *regexp.Regexp
		repl string
	}{{regexp.MustCompile(`(?i)(X-Plex-Token"?\s*[=:]\s*"?)[^&\s"<>]+`), "${1}[REDACTED]"}, {regexp.MustCompile(`(?i)(token"?\s*[=:]\s*"?)[^&\s"<>]+`), "${1}[REDACTED]"}, {regexp.MustCompile(`(?i)([\w.+-]+)@([\w.-]+\.[A-Za-z]{2,})`), "[REDACTED_EMAIL]"}, {regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "[REDACTED_IP]"}, {regexp.MustCompile(`(?i)(machineIdentifier"?\s*[=:]\s*"?)[^&\s"<>]+`), "${1}[REDACTED]"}, {regexp.MustCompile(`(?i)\b((?:\w*username|\w*friendlyName|\w*serverName|user|name)(?:"\s*:\s*"|="|=))[^&\s"<>]+`), "${1}[REDACTED]"}}
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	s = regexp.MustCompile(`(?i)(<(?:username|user|friendlyName|serverName|machineIdentifier|X-Plex-Token)[^>]*>)[^<]*(</(?:username|user|friendlyName|serverName|machineIdentifier|X-Plex-Token)>)`).ReplaceAllString(s, "${1}[REDACTED]${2}")
	s = regexp.MustCompile(`\b(?:[A-Fa-f0-9]{1,4}:){2,7}[A-Fa-f0-9]{0,4}\b`).ReplaceAllString(s, "[REDACTED_IP]")
	return []byte(s)
}

// SaveFixture stores one redacted response as a JSON fixture.
func SaveFixture(dir, name string, r Response) error {
	_, err := SaveFixtureTrimmed(dir, name, r)
	return err
}

// SaveFixtureTrimmed redacts and limits each repeated container child list to 50 items.
// It returns the number of response bodies whose listings were shortened.
func SaveFixtureTrimmed(dir, name string, r Response) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	var trimmed bool
	r.Body, trimmed = TrimBody(r.Body)
	r.Body = Redact(r.Body)
	for k, values := range r.Headers {
		if strings.EqualFold(k, "Set-Cookie") || strings.EqualFold(k, "Authorization") {
			r.Headers.Del(k)
			continue
		}
		for i := range values {
			values[i] = string(Redact([]byte(values[i])))
		}
		r.Headers[k] = values
	}
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return 0, e
	}
	if e = os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); e != nil {
		return 0, e
	}
	if trimmed {
		return 1, nil
	}
	return 0, nil
}

// TrimBody truncates XML repeated sibling elements and JSON arrays to their first 50 entries.
func TrimBody(body []byte) ([]byte, bool) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return body, false
	}
	if t[0] == '{' || t[0] == '[' {
		var v any
		if json.Unmarshal(t, &v) != nil {
			return body, false
		}
		n := trimJSON(v)
		b, e := json.Marshal(n.value)
		if e != nil {
			return body, false
		}
		return b, n.trimmed
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out bytes.Buffer
	enc := xml.NewEncoder(&out)
	type frame struct{ counts map[string]int }
	stack := []frame{}
	skip := 0
	trimmed := false
	for {
		tok, e := dec.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return body, false
		}
		if skip > 0 {
			switch tok.(type) {
			case xml.StartElement:
				skip++
			case xml.EndElement:
				skip--
			}
			continue
		}
		switch x := tok.(type) {
		case xml.StartElement:
			if len(stack) > 0 {
				p := &stack[len(stack)-1]
				if p.counts[x.Name.Local] >= 50 {
					skip = 1
					trimmed = true
					continue
				}
				p.counts[x.Name.Local]++
			}
			stack = append(stack, frame{counts: map[string]int{}})
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
		if enc.EncodeToken(tok) != nil {
			return body, false
		}
	}
	if enc.Flush() != nil {
		return body, false
	}
	return out.Bytes(), trimmed
}

type trimResult struct {
	value   any
	trimmed bool
}

func trimJSON(v any) trimResult {
	switch x := v.(type) {
	case map[string]any:
		was := false
		for k, val := range x {
			r := trimJSON(val)
			x[k] = r.value
			if r.trimmed {
				was = true
			}
		}
		return trimResult{x, was}
	case []any:
		was := len(x) > 50
		if was {
			x = x[:50]
		}
		for i, val := range x {
			r := trimJSON(val)
			x[i] = r.value
			was = was || r.trimmed
		}
		return trimResult{x, was}
	default:
		return trimResult{v, false}
	}
}

// LoadFixture reads a response fixture.
func LoadFixture(path string) (Response, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return Response{}, e
	}
	var r Response
	e = json.Unmarshal(b, &r)
	return r, e
}

// Client describes a Plex application header profile.
type Client struct {
	Name    string
	Headers http.Header
}

// Request is one logical request in the PMS probe corpus.
type Request struct {
	Name, Method, Path string
	Headers            map[string]http.Header
	Body               []byte
	RangeKind          string
}
