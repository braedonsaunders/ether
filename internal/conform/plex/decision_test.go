package plex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
)

func TestDecisionCorpusFormatAndFixtures(t *testing.T) {
	path := filepath.Join("..", "..", "..", "testdata", "conform", "plex", "decisions", "corpus.json")
	got, err := LoadDecisionCorpus(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := SeedDecisionCorpus()
	if len(got.Records) != len(seed.Records) {
		t.Fatalf("corpus has %d records, seed %d", len(got.Records), len(seed.Records))
	}
	root := filepath.Join("..", "..", "..", "testdata", "conform", "plex")
	seen := map[string]bool{}
	for _, rec := range got.Records {
		if seen[rec.ID] {
			t.Errorf("duplicate id %s", rec.ID)
		}
		seen[rec.ID] = true
		if rec.Request.Path != DecisionPath {
			t.Errorf("%s path %q", rec.ID, rec.Request.Path)
		}
		if !queryOrderPreserved(rec.Request.Query) {
			t.Errorf("%s query was reordered", rec.ID)
		}
		hasProduct, hasExtra := false, false
		for _, h := range rec.Request.Headers {
			if strings.EqualFold(h[0], "X-Plex-Product") && h[1] != "" {
				hasProduct = true
			}
			if strings.EqualFold(h[0], "X-Plex-Client-Profile-Extra") {
				hasExtra = true
			}
			if strings.Contains(strings.ToLower(h[1]), "token") && !strings.Contains(h[1], "[REDACTED]") && !strings.Contains(h[1], "{{") {
				t.Errorf("%s leaked token header %s", rec.ID, h[0])
			}
		}
		if !hasProduct {
			t.Errorf("%s missing X-Plex-Product", rec.ID)
		}
		_ = hasExtra
		ref, err := conform.LoadFixture(rec.FixturePath(root))
		if err != nil {
			t.Errorf("%s fixture: %v", rec.ID, err)
			continue
		}
		if ref.Status != 200 || len(ref.Body) == 0 {
			t.Errorf("%s fixture status %d", rec.ID, ref.Status)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wire DecisionCorpus
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Policy == "" || !strings.Contains(wire.Policy, "HYPERV03") {
		t.Fatal("corpus must record the lab capture constraint")
	}
}

func TestQueryPairsKeepWireOrder(t *testing.T) {
	got := QueryPairs("hasMDE=1&path=%2Flibrary%2Fmetadata%2F{{movieKey}}&directPlay=0&protocol=hls")
	want := [][2]string{{"hasMDE", "1"}, {"path", "/library/metadata/{{movieKey}}"}, {"directPlay", "0"}, {"protocol", "hls"}}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pair %d = %v want %v", i, got[i], want[i])
		}
	}
	req := DecisionRequest{Path: DecisionPath, Query: got, Raw: DecisionPath + "?hasMDE=1&path=%2Flibrary%2Fmetadata%2F{{movieKey}}&directPlay=0&protocol=hls"}
	if req.Target() != req.Raw {
		t.Fatal(req.Target())
	}
	again := QueryPairs(strings.TrimPrefix(req.Raw, DecisionPath+"?"))
	if again[0][0] != "hasMDE" || again[2][0] != "directPlay" {
		t.Fatalf("raw query reordered: %v", again)
	}
}

func queryOrderPreserved(query [][2]string) bool {
	if len(query) < 2 {
		return true
	}
	// The recorded browsing probes put path= before later decision flags
	// for the legacy request, and hasMDE= first for MDE requests.
	if query[0][0] == "hasMDE" || query[0][0] == "path" {
		return true
	}
	return false
}
