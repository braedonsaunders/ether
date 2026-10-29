package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	pmsconform "github.com/braedonsaunders/ether/internal/conform"
)

func TestFilterReport(t *testing.T) {
	r := pmsconform.Report{Differences: []pmsconform.Difference{
		{Path: "/MediaContainer/Video/@contentRatingAge", Kind: pmsconform.KindMissing, Blocking: true},
		{Path: "/MediaContainer/Video/@slug", Kind: pmsconform.KindMissing, Blocking: true},
		{Path: "$status", Kind: pmsconform.KindStatus, Blocking: true},
	}}
	got := filterReport(r, regexp.MustCompile(`(@|/)contentRatingAge$|status$`))
	if len(got.Differences) != 2 || got.Differences[0].Path != "/MediaContainer/Video/@slug" || got.Differences[1].Kind != pmsconform.KindStatus {
		t.Fatalf("filtered: %+v", got.Differences)
	}
	// The shipped data-gap list must compile.
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "conform", "plex", "data-gaps.regexp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := regexp.Compile(strings.TrimSpace(string(b))); err != nil {
		t.Fatal(err)
	}
}

func TestCorpusFilter(t *testing.T) {
	all := corpus("")
	dec := corpus("/transcode/universal/decision")
	if len(dec) == 0 || len(dec) >= len(all) {
		t.Fatalf("decision filter: %d of %d", len(dec), len(all))
	}
	for _, r := range dec {
		if !strings.Contains(r.Path, "/transcode/universal/decision") {
			t.Fatalf("unexpected %s", r.Path)
		}
	}
}

// fakePMS serves a tiny library: sections, one movie listing and a
// MediaContainer for everything else.
func fakePMS(t *testing.T, extra string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml;charset=utf-8")
		w.Header().Set("X-Plex-Protocol", "1.0")
		switch {
		case r.URL.Path == "/library/sections":
			w.Write([]byte(`<MediaContainer size="1"><Directory key="1" type="movie" title="Movies"/></MediaContainer>`))
		case strings.HasSuffix(r.URL.Path, "/all"):
			w.Write([]byte(`<MediaContainer size="1"><Video ratingKey="7" title="A" slug="a"` + extra + `/></MediaContainer>`))
		default:
			w.Write([]byte(`<MediaContainer size="0"/>`))
		}
	}))
}

func TestRecordOnlyKeepsDiscoveryAndManifest(t *testing.T) {
	srv := fakePMS(t, "")
	defer srv.Close()
	dir := t.TempDir()
	if _, err := record(srv.URL, "tok", dir, "web", "", 5*time.Second, 2); err != nil {
		t.Fatal(err)
	}
	disc := filepath.Join(dir, "web", "_discovery.json")
	before, err := os.ReadFile(disc)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a manifest note for another endpoint; a partial recording keeps it.
	var m map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	json.Unmarshal(b, &m)
	m["trimmedResponses"] = []string{"web /hubs trimmed to first 50 entries"}
	b, _ = json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644)
	os.WriteFile(disc, append(bytes.TrimSpace(before), '\n'), 0o644)

	if _, err := record(srv.URL, "tok", dir, "web", "/identity", 5*time.Second, 2); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(disc)
	if !bytes.Equal(bytes.TrimSpace(after), bytes.TrimSpace(before)) {
		t.Fatalf("partial recording rewrote discovery:\n%s\n%s", before, after)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "manifest.json"))
	if !strings.Contains(string(b), "web /hubs trimmed") {
		t.Fatalf("manifest lost earlier notes: %s", b)
	}
}

func TestCompareSummaryAgainstFixtures(t *testing.T) {
	ref := fakePMS(t, "")
	defer ref.Close()
	dir := t.TempDir()
	if _, err := record(ref.URL, "tok", dir, "web", "", 5*time.Second, 2); err != nil {
		t.Fatal(err)
	}
	// The target lacks nothing but adds an attribute in listings.
	target := fakePMS(t, ` extra="1"`)
	defer target.Close()
	rep, err := compare(dir, target.URL, "tok", "web", "", 5*time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	emit(&out, rep, "text", true, false)
	s := out.String()
	if !strings.Contains(s, "/MediaContainer/Video/@extra") || !strings.Contains(s, "| **total** |") {
		t.Fatalf("summary:\n%s", s)
	}
	if rep.Blocking() != 2 { // xml and json listing
		t.Fatalf("blocking = %d: %+v", rep.Blocking(), rep.Differences)
	}
	out.Reset()
	emit(&out, rep, "json", true, false)
	var js struct {
		Endpoints []pmsconform.EndpointSummary `json:"endpoints"`
	}
	if err := json.Unmarshal(out.Bytes(), &js); err != nil || len(js.Endpoints) != len(corpus("")) {
		t.Fatalf("json summary: %v %d", err, len(js.Endpoints))
	}
}
