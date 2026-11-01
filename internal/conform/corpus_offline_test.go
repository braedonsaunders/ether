package conform_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	"github.com/braedonsaunders/ether/internal/conform/silo"
)

// The recorded corpus is the offline oracle until front-end tests can exercise
// the same probes against Ether. Never silently skip a missing fixture.
func TestRecordedMediaBrowserCorpora(t *testing.T) {
	for _, protocol := range []string{"jellyfin", "emby"} {
		t.Run(protocol, func(t *testing.T) {
			p, err := mediabrowser.Load(protocol, filepath.Join("profiles", protocol))
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join("..", "..", "testdata", "conform", protocol)
			manifest, err := os.ReadFile(filepath.Join(root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var m struct {
				Protocol      string            `json:"protocol"`
				FixtureCounts map[string]int    `json:"fixtureCounts"`
				RequestErrors []json.RawMessage `json:"requestErrors"`
			}
			if err = json.Unmarshal(manifest, &m); err != nil {
				t.Fatal(err)
			}
			if m.Protocol != protocol || len(m.RequestErrors) != 0 {
				t.Fatalf("invalid corpus manifest")
			}
			for client := range p.Clients() {
				requests := p.Corpus(client)
				if m.FixtureCounts[client] != len(requests) {
					t.Errorf("%s: manifest count %d, corpus %d", client, m.FixtureCounts[client], len(requests))
				}
				for _, req := range requests {
					name := p.FixtureName(client, req, true)
					r, err := conform.LoadFixture(filepath.Join(root, client, name+".json"))
					if err != nil {
						t.Errorf("%s: %v", name, err)
						continue
					}
					if (r.Status < 200 || r.Status >= 400) && !(req.FixtureOnly && req.ExpectedStatus == r.Status) {
						t.Errorf("%s: unexpected status %d", name, r.Status)
					}
					if r.Kind == "binary-range" {
						if len(r.Body) != 0 || r.SHA256 == "" {
							t.Errorf("%s: invalid binary fixture", name)
						}
						if r.ReferenceDefect == nil {
							if req.FixtureOnly && (r.Truncated || r.RequestRange == "") {
								continue
							}
							if err := conform.ValidateBinary(r); err != nil {
								t.Errorf("%s: %v", name, err)
							}
						} else if protocol != "emby" || r.ReferenceDefect.ID != "emby-suffix-range-ignored" || !r.Truncated {
							t.Errorf("%s: unexpected reference defect", name)
						}
					}
				}
			}
		})
	}
}

func TestRecordedSiloCorpus(t *testing.T) {
	p, err := silo.Load(filepath.Join("profiles", "silo"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..", "testdata", "conform", "silo")
	b, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Protocol      string            `json:"protocol"`
		FixtureCounts map[string]int    `json:"fixtureCounts"`
		RequestErrors []json.RawMessage `json:"requestErrors"`
	}
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Protocol != "silo" || len(m.RequestErrors) != 0 {
		t.Fatal("invalid Silo manifest")
	}
	for client := range p.Clients() {
		requests := p.Corpus(client)
		if m.FixtureCounts[client] != len(requests) {
			t.Errorf("%s: fixture count does not match profile", client)
		}
		for _, req := range requests {
			name := p.FixtureName(client, req, true)
			r, err := conform.LoadFixture(filepath.Join(root, client, name+".json"))
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if (r.Status < 200 || r.Status >= 400) && !(req.FixtureOnly && req.ExpectedStatus == r.Status) {
				t.Errorf("%s: status %d", name, r.Status)
			}
			if strings.Contains(req.Name, "playback plan") {
				var body struct {
					Outcome string `json:"outcome"`
				}
				if err = json.Unmarshal(r.Body, &body); err != nil {
					t.Error(err)
					continue
				}
				if body.Outcome != "playable" {
					t.Errorf("%s: outcome %q", name, body.Outcome)
				}
			}
		}
	}
}
