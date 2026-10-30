package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

func TestBaselineNondeterminismRequiresExactPairAndContext(t *testing.T) {
	var entries []baselineNoise
	if err := json.Unmarshal(baselineNondeterminism, &entries); err != nil || len(entries) != 30 {
		t.Fatalf("entries=%d error=%v", len(entries), err)
	}
	for _, e := range entries {
		if e.Viewer != "owner" || len(e.Rows) != 2 || e.Rows[0] != "home.because.1000000000010001" || e.Rows[1] != "home.because.1000000000010003" {
			t.Fatalf("unexpected baseline signature: %+v", e)
		}
	}
	old := baselineNondeterminism
	defer func() { baselineNondeterminism = old }()
	a := capture{viewer: "owner", client: "web", path: "/hubs?count=5", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"text/xml"}}, Body: []byte("first")}}
	b := a
	b.response.Body = []byte("second")
	entry := baselineNoise{Viewer: a.viewer, Client: a.client, Path: a.path, First: signature(a.response), Second: signature(b.response)}
	baselineNondeterminism, _ = json.Marshal([]baselineNoise{entry})
	for _, pair := range [][2]capture{{a, b}, {b, a}} {
		if ok, err := baselineNondeterministic(pair[0], pair[1]); err != nil || !ok {
			t.Fatalf("exact pair: %v %v", ok, err)
		}
	}
	b.response.Body = []byte("unrelated")
	if ok, _ := baselineNondeterministic(a, b); ok {
		t.Fatal("unrelated body was accepted")
	}
	b.response.Body = []byte("second")
	a.viewer = "adult"
	if ok, _ := baselineNondeterministic(a, b); ok {
		t.Fatal("unrestricted context was widened")
	}
	a.viewer, a.path = "owner", "/hubs?count=2"
	if ok, _ := baselineNondeterministic(a, b); ok {
		t.Fatal("different route was accepted")
	}
}

func TestCounterfactualDoesNotHideBaselineNoise(t *testing.T) {
	t.Setenv("ETHER_PARITY_POLICY_COUNTERFACTUAL", "1")
	old := baselineNondeterminism
	defer func() { baselineNondeterminism = old }()
	a := capture{viewer: "owner", client: "web", path: "/hubs?count=5", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"text/xml"}}, Body: []byte("first")}}
	b := a
	b.response.Body = []byte("second")
	entry := baselineNoise{Viewer: a.viewer, Client: a.client, Path: a.path, First: signature(a.response), Second: signature(b.response)}
	baselineNondeterminism, _ = json.Marshal([]baselineNoise{entry})
	var out bytes.Buffer
	n, err := diffTranscripts(transcript{captures: []capture{a}}, transcript{captures: []capture{b}}, &out)
	if err != nil || n != 1 {
		t.Fatalf("counterfactual hid a difference: n=%d err=%v report=%s", n, err, out.String())
	}
}
