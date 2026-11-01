package capture

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type capturedProfile struct {
	Protocol string                      `json:"protocol"`
	Client   string                      `json:"client"`
	Source   string                      `json:"source"`
	Headers  map[string]string           `json:"headers"`
	Flows    map[string][]profileRequest `json:"flows"`
}
type profileRequest struct {
	Name           string            `json:"name"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           json.RawMessage   `json:"body,omitempty"`
	BodyBase64     string            `json:"body_base64,omitempty"`
	RangeKind      string            `json:"range_kind,omitempty"`
	Binary         bool              `json:"binary,omitempty"`
	FixtureOnly    bool              `json:"fixture_only,omitempty"`
	ExpectedStatus int               `json:"expected_status,omitempty"`
}

// ProfileFromCapture converts an ordered capture into an explicitly captured profile.
// It refuses incomplete text requests, since they cannot be replayed faithfully.
func ProfileFromCapture(dir, protocol, client, out string) error {
	if !slug.MatchString(client) {
		return fmt.Errorf("invalid client name")
	}
	switch protocol {
	case "jellyfin", "emby", "silo":
	default:
		return fmt.Errorf("unknown protocol %q", protocol)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	p := capturedProfile{Protocol: protocol, Client: client, Source: "captured", Headers: map[string]string{}, Flows: map[string][]profileRequest{"captured": {}}}
	last := 0
	for _, file := range files {
		b, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		var entry Entry
		if e = json.Unmarshal(b, &entry); e != nil {
			return fmt.Errorf("%s: %w", file, e)
		}
		if entry.Protocol != protocol || entry.Client != client {
			return fmt.Errorf("%s: capture protocol/client mismatch", file)
		}
		if entry.Sequence != last+1 {
			return fmt.Errorf("capture sequence is not increasing")
		}
		last = entry.Sequence
		if entry.RequestTruncated || entry.ResponseTruncated {
			return fmt.Errorf("sequence %d has an incomplete text capture", entry.Sequence)
		}
		req := profileRequest{Name: fmt.Sprintf("captured-%06d", entry.Sequence), Method: entry.Request.Method, Path: entry.Request.Path, Headers: map[string]string{}}
		for k, vs := range entry.Request.Headers {
			if len(vs) > 0 {
				req.Headers[k] = strings.Join(vs, ", ")
			}
		}
		if entry.Response.Kind == "binary-range" {
			req.Binary = true
			if entry.Request.Headers.Get("Range") != "" {
				req.RangeKind = "captured"
			}
		}
		if len(entry.Request.Body) > 0 {
			if json.Valid(entry.Request.Body) {
				req.Body = templateBody(entry.Request.Body)
			} else {
				body := entry.Request.Body
				if isForm(entry.Request.Headers.Get("Content-Type")) {
					body = templateForm(body)
				}
				req.BodyBase64 = base64.StdEncoding.EncodeToString(body)
			}
		}
		p.Flows["captured"] = append(p.Flows["captured"], req)
	}
	if len(p.Flows["captured"]) == 0 {
		return fmt.Errorf("no capture entries")
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return err
	}
	return os.WriteFile(out, append(b, '\n'), 0644)
}

// templateBody turns redacted credential fields into explicit replay variables.
func templateBody(body []byte) json.RawMessage {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if str, ok := val.(string); ok && str == "[REDACTED]" {
					n := strings.ToLower(k)
					switch {
					case n == "username":
						x[k] = "${username}"
					case n == "pw" || strings.Contains(n, "password"):
						x[k] = "${password}"
					case strings.Contains(n, "token") || n == "api_key":
						x[k] = "${access_token}"
					default:
						x[k] = "${redacted}"
					}
				} else {
					x[k] = walk(val)
				}
			}
		case []any:
			for i, val := range x {
				x[i] = walk(val)
			}
		}
		return v
	}
	b, _ := json.Marshal(walk(v))
	return b
}

func templateForm(body []byte) []byte {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil
	}
	for k, vs := range values {
		for i, v := range vs {
			if v != "[REDACTED]" {
				continue
			}
			n := strings.ToLower(k)
			switch {
			case n == "username":
				vs[i] = "${username}"
			case n == "pw" || strings.Contains(n, "password"):
				vs[i] = "${password}"
			case strings.Contains(n, "token") || n == "api_key":
				vs[i] = "${access_token}"
			default:
				vs[i] = "${redacted}"
			}
		}
		values[k] = vs
	}
	encoded := values.Encode()
	encoded = strings.ReplaceAll(encoded, "%24%7B", "${")
	encoded = strings.ReplaceAll(encoded, "%7D", "}")
	return []byte(encoded)
}
