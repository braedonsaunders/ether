package plex

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
)

// Hubs, search and sections were recorded but only parsed for status/type.
// These tests load every client/encoding fixture and require the
// load-bearing fields PMS actually sent.
func TestRecordedHubsSearchSectionsFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", "testdata", "conform", "plex")
	var probes []Request
	for _, req := range Corpus() {
		if browseFixture(req.Path) {
			probes = append(probes, req)
		}
	}
	if len(probes) < 8 {
		t.Fatalf("expected hubs/search/sections probes, got %d", len(probes))
	}
	for _, profile := range []string{"web", "ios", "appletv"} {
		for _, req := range probes {
			for _, jsonMode := range []bool{false, true} {
				name := conform.FixtureName(profile, req.Path, jsonMode)
				t.Run(profile+"/"+name, func(t *testing.T) {
					ref, err := conform.LoadFixture(filepath.Join(root, profile, name+".json"))
					if err != nil {
						t.Fatalf("recorded fixture missing: %v", err)
					}
					if ref.Status != 200 {
						if ref.Status == 400 && strings.Contains(req.Path, "/hubs/search?query=") && !strings.Contains(req.Path, "query=burbs") {
							return
						}
						t.Fatalf("status %d", ref.Status)
					}
					if len(ref.Body) == 0 {
						t.Fatal("empty body")
					}
					if n := Compare(ref, ref).Blocking(); n != 0 {
						t.Fatalf("fixture does not compare to itself: %+v", Compare(ref, ref).Differences)
					}
					present := namedLeaves(t, ref.Body)
					required := requiredLoadBearing(req.Path)
					for _, field := range required {
						if !present[field] {
							t.Errorf("PMS fixture omitted load-bearing %q", field)
						}
					}
				})
			}
		}
	}
}

func browseFixture(path string) bool {
	return strings.HasPrefix(path, "/hubs") || strings.HasPrefix(path, "/library/search") || strings.HasPrefix(path, "/library/sections")
}

func requiredLoadBearing(path string) []string {
	switch {
	case strings.HasPrefix(path, "/hubs"):
		return []string{"hubIdentifier", "key", "type", "size"}
	case strings.HasPrefix(path, "/library/search"):
		return []string{"size"}
	case path == "/library/sections":
		return []string{"size", "type", "key", "allowSync"}
	case strings.Contains(path, "/prefs"):
		return []string{"size"}
	default:
		return []string{"size", "viewGroup", "viewMode", "allowSync"}
	}
}

func namedLeaves(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	b := bytes.TrimSpace(body)
	if len(b) == 0 {
		return out
	}
	if b[0] == '{' || b[0] == '[' {
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		var walk func(any)
		walk = func(x any) {
			switch n := x.(type) {
			case map[string]any:
				for k, val := range n {
					out[k] = true
					walk(val)
				}
			case []any:
				for _, val := range n {
					walk(val)
				}
			}
		}
		walk(v)
		return out
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		out[se.Name.Local] = true
		for _, a := range se.Attr {
			out[a.Name.Local] = true
		}
	}
}
