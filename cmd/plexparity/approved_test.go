package main

import (
	"encoding/json"
	"testing"
)

func TestApprovedDifferencesAreNarrow(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedSearch, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 30 {
		t.Fatalf("got %d signatures, want 30", len(entries))
	}
	for _, e := range entries {
		if e.Viewer != "teen" && e.Viewer != "child" {
			t.Fatalf("unrestricted viewer approved: %+v", e)
		}
		if e.Path != "/hubs/search?query=fixture&limit=10&includeCollections=1" && e.Path != "/library/search?query=fixture&limit=10&searchTypes=movies%2Ctv&includeMetadata=1" {
			t.Fatalf("unexpected route: %s", e.Path)
		}
		if len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("invalid signatures: %+v", e)
		}
	}
	a := capture{viewer: "owner"}
	if ok, err := approved(a, a); err != nil || ok {
		t.Fatalf("unrestricted approval: %v %v", ok, err)
	}
}
