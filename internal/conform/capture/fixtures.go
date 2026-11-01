package capture

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
)

// FixturesFromCapture writes fixtures from observed reference responses without replay.
// Captures must contain origin markers; older ambiguous captures must be recaptured.
func FixturesFromCapture(dir, protocol, client, profileRoot, fixtureRoot string) error {
	if !slug.MatchString(client) {
		return fmt.Errorf("invalid client name")
	}
	if protocol != "jellyfin" && protocol != "emby" && protocol != "silo" {
		return fmt.Errorf("invalid protocol")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	p := capturedProfile{Protocol: protocol, Client: client, Source: "captured", Headers: map[string]string{}, Flows: map[string][]profileRequest{"captured": {}}}
	var responses []conform.Response
	seen := map[string]bool{}
	keys := map[string]string{}
	for i, file := range files {
		b, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		var entry Entry
		if e = json.Unmarshal(b, &entry); e != nil {
			return e
		}
		if entry.Sequence != i+1 || entry.Protocol != protocol || entry.Client != client {
			return fmt.Errorf("invalid capture sequence or identity: %s", file)
		}
		if entry.ResponseOrigin == "proxy-error" {
			continue
		}
		if entry.ResponseOrigin != "reference" {
			return fmt.Errorf("sequence %d has ambiguous response origin", entry.Sequence)
		}
		if entry.RequestTruncated || entry.ResponseTruncated {
			return fmt.Errorf("sequence %d has incomplete text", entry.Sequence)
		}
		req := profileRequest{Method: entry.Request.Method, Path: entry.Request.Path, Headers: map[string]string{}}
		for k, vs := range entry.Request.Headers {
			if len(vs) > 0 {
				req.Headers[k] = websocketTicket.ReplaceAllStringFunc(strings.Join(vs, ", "), func(string) string { return "silo.ticket.${ticket}" })
			}
		}
		req.FixtureOnly = req.Method != http.MethodGet && req.Method != http.MethodHead
		if entry.Response.Status < 200 || entry.Response.Status >= 400 {
			req.ExpectedStatus = entry.Response.Status
			req.FixtureOnly = true
		}
		if entry.Response.Kind == "binary-range" {
			req.Binary = true
			req.RangeKind = "captured"
			req.FixtureOnly = true
			entry.Response.RangeKind = "captured"
			entry.Response.RequestRange = entry.Request.Headers.Get("Range")
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
		key, _ := json.Marshal(req)
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		req.Name = fmt.Sprintf("captured-%06d", entry.Sequence)
		p.Flows["captured"] = append(p.Flows["captured"], req)
		entry.Response = (&mediabrowser.Protocol{}).Redact(entry.Response)
		entry.Response.Body = redactObjectKeys(entry.Response.Body, keys)
		responses = append(responses, entry.Response)
	}
	if len(responses) == 0 {
		return fmt.Errorf("no reference responses")
	}
	profileDir := filepath.Join(profileRoot, protocol)
	if err = os.MkdirAll(profileDir, 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(profileDir, client+".json"), append(b, '\n'), 0644); err != nil {
		return err
	}
	loaded, err := mediabrowser.Load(protocol, profileDir)
	if err != nil {
		return err
	}
	requests := loaded.Corpus(client)
	if len(requests) != len(responses) {
		return fmt.Errorf("capture corpus expansion mismatch")
	}
	root := filepath.Join(fixtureRoot, protocol)
	for i, req := range requests {
		if err = conform.SaveFixture(filepath.Join(root, client), loaded.FixtureName(client, req, true), responses[i]); err != nil {
			return err
		}
	}
	manifestPath := filepath.Join(root, "manifest.json")
	manifest := map[string]json.RawMessage{}
	if b, err = os.ReadFile(manifestPath); err == nil {
		if err = json.Unmarshal(b, &manifest); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	counts := map[string]int{}
	if v := manifest["fixtureCounts"]; len(v) > 0 {
		if err = json.Unmarshal(v, &counts); err != nil {
			return err
		}
	}
	counts[client] = len(requests)
	put := func(k string, v any) { manifest[k], _ = json.Marshal(v) }
	put("fixtureCounts", counts)
	put("protocol", protocol)
	if _, ok := manifest["requestErrors"]; !ok {
		put("requestErrors", nil)
	}
	put("capturePolicy", "Captured reference responses; proxy errors excluded; mutating requests fixture-only; listings trimmed to 50 entries; binary bytes omitted.")
	b, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(b, '\n'), 0644)
}

// Older captures may contain opaque image tags as JSON object keys.
func redactObjectKeys(body []byte, keys map[string]string) []byte {
	var value any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return body
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			out := make(map[string]any, len(x))
			ordered := make([]string, 0, len(x))
			for key := range x {
				ordered = append(ordered, key)
			}
			sort.Strings(ordered)
			for _, key := range ordered {
				name := key
				if opaqueID.MatchString(key) {
					if keys[key] == "" {
						keys[key] = fmt.Sprintf("${capture_key_%d}", len(keys)+1)
					}
					name = keys[key]
				}
				out[name] = walk(x[key])
			}
			return out
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		}
		return v
	}
	b, err := json.Marshal(walk(value))
	if err != nil {
		return body
	}
	return b
}
