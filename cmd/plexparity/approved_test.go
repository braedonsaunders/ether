package main

import (
	"bytes"
	"encoding/json"
	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	"net/http"
	"testing"
)

func TestApprovedDifferencesAreNarrow(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedSearch, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 260 {
		t.Fatalf("got %d signatures, want 260", len(entries))
	}
	for _, e := range entries {
		if e.Viewer != "teen" && e.Viewer != "child" {
			t.Fatalf("unrestricted viewer approved: %+v", e)
		}
		if e.Path != "/hubs/search?query=fixture&limit=10&includeCollections=1" && e.Path != "/library/search?query=fixture&limit=10&searchTypes=movies%2Ctv&includeMetadata=1" && e.Path != "/search?query=fixture" && e.Path != "/hubs/search/voice?query=fixture" && e.Path != "/hubs/metadata/{{movieKey}}/postplay" {
			t.Fatalf("unexpected route: %s", e.Path)
		}
		if len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("invalid signatures: %+v", e)
		}
	}
	a := capture{viewer: "owner"}
	if ok, err := approved(a, a); err != nil || ok != "" {
		t.Fatalf("unrestricted approval: %v %v", ok, err)
	}
	a = capture{viewer: "teen", client: "web", path: "/search?query=fixture", response: protocol.Response{Status: 200, Body: []byte("unreviewed")}}
	b := a
	b.response.Body = []byte("different")
	if ok, err := approved(a, b); err != nil || ok != "" {
		t.Fatalf("route and viewer alone approved an unreviewed pair: %v %v", ok, err)
	}
}

func TestCounterfactualDoesNotHideApprovedPairs(t *testing.T) {
	t.Setenv("ETHER_PARITY_POLICY_COUNTERFACTUAL", "1")
	old := approvedSearch
	defer func() { approvedSearch = old }()
	a := capture{viewer: "teen", client: "web", path: "/search?query=fixture", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte("first")}}
	b := a
	b.response.Body = []byte("second")
	entry := approvedDifference{Viewer: a.viewer, Client: a.client, Path: a.path, Baseline: signature(a.response), Candidate: signature(b.response), Commit: "c82a7830"}
	approvedSearch, _ = json.Marshal([]approvedDifference{entry})
	var out bytes.Buffer
	n, err := diffTranscripts(transcript{captures: []capture{a}}, transcript{captures: []capture{b}}, &out)
	if err != nil || n != 1 {
		t.Fatalf("counterfactual hid an approved pair: n=%d err=%v report=%s", n, err, out.String())
	}
}

func TestApprovedHeadersOnlyPermitValidBodyLengths(t *testing.T) {
	a := capture{response: protocol.Response{Headers: http.Header{"Content-Length": {"3"}, "Cache-Control": {"no-cache"}}, Body: []byte("old")}}
	b := capture{response: protocol.Response{Headers: http.Header{"Content-Length": {"4"}, "Cache-Control": {"no-cache"}}, Body: []byte("new!")}}
	if !approvedHeaders(a, b) {
		t.Fatal("expected matching headers with valid lengths")
	}
	b.response.Headers.Set("Cache-Control", "public")
	if approvedHeaders(a, b) {
		t.Fatal("unrelated header difference accepted")
	}
	b.response.Headers.Set("Cache-Control", "no-cache")
	b.response.Headers.Set("Content-Length", "99")
	if approvedHeaders(a, b) {
		t.Fatal("invalid content length accepted")
	}
}
