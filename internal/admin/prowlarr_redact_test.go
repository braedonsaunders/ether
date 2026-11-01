package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestProwlarrAPIKeyRedactsAndRestoresForTheSameURL(t *testing.T) {
	prev := config.Config{}
	prev.Prowlarr = config.Prowlarr{URL: "http://prowlarr.lan:9696", APIKey: "secret", Tags: []int{3}}
	red := RedactConfig(prev)
	if red.Prowlarr.APIKey != RedactedSecret {
		t.Fatalf("redacted key = %q", red.Prowlarr.APIKey)
	}
	raw, err := json.Marshal(red)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("secret leaked: %s", raw)
	}
	got, errs := UnredactConfig(red, prev)
	if len(errs) != 0 || got.Prowlarr.APIKey != "secret" || len(got.Prowlarr.Tags) != 1 {
		t.Fatalf("restored key=%q errs=%v tags=%v", got.Prowlarr.APIKey, errs, got.Prowlarr.Tags)
	}
	moved := red
	moved.Prowlarr.URL = "http://other.lan:9696"
	got, errs = UnredactConfig(moved, prev)
	if got.Prowlarr.APIKey != "" || len(errs) == 0 {
		t.Fatalf("moved URL key=%q errs=%v", got.Prowlarr.APIKey, errs)
	}
}
