package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResolverDropsGlobalAllowCam(t *testing.T) {
	var got Resolver
	if err := json.Unmarshal([]byte(`{"allowCam":true,"healthSamples":4}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.HealthSamples != 4 {
		t.Fatalf("healthSamples %d", got.HealthSamples)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "allowCam") {
		t.Fatalf("global allowCam still serialized: %s", raw)
	}
}
