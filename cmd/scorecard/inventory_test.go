package main

import (
	"encoding/json"
	"testing"
)

func TestInventoryUsesMethodAndNormalizesParameters(t *testing.T) {
	spec := []byte(`{"paths":{"/System/Info/Public":{"get":{}},"/MediaSegments/{differentId}":{"get":{},"post":{}},"/SyncPlay/New":{"post":{}}}}`)
	report, e := buildInventory("jellyfin", spec)
	if e != nil {
		t.Fatal(e)
	}
	got := map[string]bool{}
	for _, r := range report.Routes {
		got[r.Method+" "+r.Path] = r.Registered
	}
	if !got["GET /System/Info/Public"] || !got["GET /MediaSegments/{differentId}"] || got["POST /MediaSegments/{differentId}"] || got["POST /SyncPlay/New"] {
		b, _ := json.Marshal(report)
		t.Fatal(string(b))
	}
	if _, e = buildInventory("unknown", spec); e == nil {
		t.Fatal("unknown protocol accepted")
	}
}
