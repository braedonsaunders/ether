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
	if len(entries) != 340 {
		t.Fatalf("got %d signatures, want 340", len(entries))
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

func TestScopeApprovalRequiresExactResponsePair(t *testing.T) {
	old := approvedScopes
	defer func() { approvedScopes = old }()
	a := capture{viewer: "owner", client: "web", path: "/hubs/home/recentlyAdded?type=15", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte("mixed")}}
	b := a
	b.response.Body = []byte("empty")
	entry := approvedDifference{Viewer: a.viewer, Client: a.client, Path: a.path, Baseline: signature(a.response), Candidate: signature(b.response), Commit: "19bdfcd1"}
	approvedScopes, _ = json.Marshal([]approvedDifference{entry})
	if got, err := approved(a, b); err != nil || got != entry.Commit {
		t.Fatalf("exact pair: %q %v", got, err)
	}
	b.response.Body = []byte("other")
	if got, err := approved(a, b); err != nil || got != "" {
		t.Fatalf("unreviewed pair approved: %q %v", got, err)
	}
}

func TestApprovedScopeRegistryIsExact(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedScopes, &entries); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"/hubs/home/recentlyAdded?type=15":                           true,
		"/hubs/home/recentlyAdded?type=999":                          true,
		"/hubs/home/recentlyAdded?type=1%2C4":                        true,
		"/hubs/home/recentlyAdded?type=2%2C4&contentDirectoryID=2":   true,
		"/hubs/home/recentlyAdded?contentDirectoryID=1":              true,
		"/hubs/home/recentlyAdded?contentDirectoryID=2":              true,
		"/hubs/home/recentlyAdded?pinnedContentDirectoryID=1":        true,
		"/hubs/home/recentlyAdded?pinnedContentDirectoryID=2":        true,
		"/hubs/home/recentlyAdded?type=1&contentDirectoryID=2":       true,
		"/hubs/home/recentlyAdded?type=2&pinnedContentDirectoryID=1": true,
	}
	for _, e := range entries {
		if !allowed[e.Path] || e.Viewer == "" || e.Client == "" || e.Commit == "" || len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("scope approval is not an exact reviewed pair: %+v", e)
		}
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

func TestScopeApprovalAcceptsOnlyExactCompressionHeaderChange(t *testing.T) {
	a := capture{response: protocol.Response{Headers: http.Header{"Vary": {"Accept-Encoding"}, "Content-Length": {"3"}, "Cache-Control": {"no-cache"}}, Body: []byte("old")}}
	b := capture{response: protocol.Response{Headers: http.Header{"Content-Length": {"4"}, "Cache-Control": {"no-cache"}}, Body: []byte("new!")}}
	if !approvedScopeHeaders(a, b) {
		t.Fatal("expected exact Vary change")
	}
	b.response.Headers.Set("Cache-Control", "public")
	if approvedScopeHeaders(a, b) {
		t.Fatal("unrelated header difference accepted")
	}
	b.response.Headers.Set("Cache-Control", "no-cache")
	a.response.Headers.Set("Vary", "Origin")
	if approvedScopeHeaders(a, b) {
		t.Fatal("unreviewed Vary value accepted")
	}
}
