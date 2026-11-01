package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/capture"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
)

func TestDecisionsFromCaptureCommand(t *testing.T) {
	dir := t.TempDir()
	entry := capture.Entry{
		Sequence:       1,
		Protocol:       "plex",
		Client:         "web",
		ResponseOrigin: "reference",
		Request: capture.Request{
			Method: "GET",
			Path:   conformplex.DecisionPath + "?hasMDE=1&path=/library/metadata/{{ratingKey}}&directPlay=1",
			Headers: map[string][]string{
				"Accept":                      {"application/json"},
				"X-Plex-Product":              {"Plex Web"},
				"X-Plex-Client-Profile-Extra": {"add-direct-play-profile(type=videoCodec&name=hevc)"},
			},
		},
		Response: conform.Response{Status: 200},
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "00000001.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "corpus.json")
	if err = decisionsFromCapture([]string{"-capture", dir, "-client", "web", "-out", out}); err != nil {
		t.Fatal(err)
	}
	c, err := conformplex.LoadDecisionCorpus(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Records) != 1 || c.Records[0].Request.Query[0][0] != "hasMDE" {
		t.Fatalf("%+v", c.Records)
	}
}
