package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
)

// Protocol implements the Plex conformance dialect.
type Protocol struct{}

var _ conform.Protocol = Protocol{}

func (Protocol) Clients() map[string]conform.Client { return Clients() }
func (Protocol) Corpus(string) []conform.Request    { return Corpus() }
func (Protocol) Formats() []bool                    { return []bool{false, true} }
func (Protocol) Discover(ctx context.Context, c *http.Client, base, token, profile string, align *conform.DiscoveredIDs) conform.DiscoveredIDs {
	return Discover(ctx, c, base, token, profile, align)
}
func (Protocol) Fetch(ctx context.Context, c *http.Client, base, token string, r conform.Request, profile string, vars map[string]string, jsonMode bool) (conform.Response, error) {
	return Fetch(ctx, c, base, token, r, profile, vars, jsonMode)
}
func (Protocol) FixtureName(profile string, r conform.Request, jsonMode bool) string {
	return conform.FixtureName(profile, r.Path, jsonMode)
}
func (Protocol) Compare(a, b conform.Response) conform.Report { return Compare(a, b) }

func (Protocol) Redact(r conform.Response) conform.Response { return r }

func (Protocol) IgnorePaths() []string { return nil }

// Compare preserves the Plex-specific response header contract and
// promotes load-bearing value mismatches (including hub #order) to
// blocking. Shared Compare leaves KindValue informational so other
// dialects are not gated on this list.
func Compare(a, b conform.Response) conform.Report {
	headers := []string{"Content-Type", "X-Plex-Protocol", "Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods"}
	r := conform.CompareHeaders(a, b, headers)
	if d, ok := hubOrderDiff(a.Body, b.Body); ok {
		r.Differences = append(r.Differences, d)
	}
	for i := range r.Differences {
		r.Differences[i].Blocking = plexBlocking(r.Differences[i])
	}
	return r
}

func plexBlocking(d conform.Difference) bool {
	if d.Kind != conform.KindValue {
		return true
	}
	if strings.HasSuffix(d.Path, "#order") {
		return strings.Contains(d.Path, "/Hub")
	}
	return conform.LoadBearing(d.Path)
}

func hubOrderDiff(ref, tgt []byte) (conform.Difference, bool) {
	a, b := hubIdentifiers(ref), hubIdentifiers(tgt)
	if len(a) < 2 || len(b) < 2 {
		return conform.Difference{}, false
	}
	if sameStringSet(a, b) && !stringSliceEqual(a, b) {
		path := "/MediaContainer/Hub/#order"
		if jsonBody(ref) || jsonBody(tgt) {
			path = "$/MediaContainer/Hub[]/#order"
		}
		return conform.Difference{
			Path: path, Kind: conform.KindValue,
			Reference: strings.Join(a, ","), Target: strings.Join(b, ","),
			Count: len(a), Reason: "hub identifier order", Blocking: true,
		}, true
	}
	return conform.Difference{}, false
}

func jsonBody(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && (t[0] == '{' || t[0] == '[')
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca, cb := map[string]int{}, map[string]int{}
	for _, v := range a {
		ca[v]++
	}
	for _, v := range b {
		cb[v]++
	}
	if len(ca) != len(cb) {
		return false
	}
	for k, n := range ca {
		if cb[k] != n {
			return false
		}
	}
	return true
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hubIdentifiers(body []byte) []string {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return nil
	}
	if t[0] == '{' || t[0] == '[' {
		return jsonHubIdentifiers(t)
	}
	if t[0] != '<' {
		return nil
	}
	dec := xml.NewDecoder(bytes.NewReader(t))
	var out []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			return out
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Hub" {
			continue
		}
		id := ""
		for _, a := range se.Attr {
			if a.Name.Local == "hubIdentifier" {
				id = a.Value
				break
			}
		}
		if id != "" {
			out = append(out, id)
		}
	}
}

func jsonHubIdentifiers(body []byte) []string {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	mc, _ := root["MediaContainer"].(map[string]any)
	if mc == nil {
		return nil
	}
	raw, ok := mc["Hub"]
	if !ok {
		raw = mc["Hubs"]
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		obj, _ := item.(map[string]any)
		if obj == nil {
			continue
		}
		if id, _ := obj["hubIdentifier"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out
}
