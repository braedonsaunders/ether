package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// These exact full-response pairs were independently observed in self-runs
// of main 6079a1d9 and Prism e59a29a5. The only changes swap fixture movies
// 027 and 031 between the two named Because You Watched shelves. Full-body
// signatures ensure unrelated changes on the same route are never hidden.
//
//go:embed baseline_nondeterminism.json
var baselineNondeterminism []byte

type baselineNoise struct {
	Viewer string            `json:"viewer"`
	Client string            `json:"client"`
	JSON   bool              `json:"json"`
	Path   string            `json:"path"`
	Rows   []string          `json:"rows"`
	First  responseSignature `json:"first"`
	Second responseSignature `json:"second"`
}

func baselineNondeterministic(a, b capture) (bool, error) {
	if a.viewer != "owner" {
		return false, nil
	}
	var entries []baselineNoise
	if err := json.Unmarshal(baselineNondeterminism, &entries); err != nil {
		return false, fmt.Errorf("invalid baseline nondeterminism: %w", err)
	}
	for _, e := range entries {
		if e.Viewer != a.viewer || e.Client != a.client || e.JSON != a.json || e.Path != a.path {
			continue
		}
		x, y := signature(a.response), signature(b.response)
		if ((x == e.First && y == e.Second) || (x == e.Second && y == e.First)) && approvedHeaders(a, b) {
			return true, nil
		}
	}
	return false, nil
}
