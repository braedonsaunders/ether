package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// c82a7830 deliberately filters restricted viewers before the search limit
// and backfills the visible results. These exact fixture response pairs are
// approved; route/viewer matching alone could hide an unrelated regression.
//
//go:embed approved_search.json
var approvedSearch []byte

type responseSignature struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
}
type approvedDifference struct {
	Viewer    string            `json:"viewer"`
	Client    string            `json:"client"`
	JSON      bool              `json:"json"`
	Path      string            `json:"path"`
	Baseline  responseSignature `json:"baseline"`
	Candidate responseSignature `json:"candidate"`
	Commit    string            `json:"commit"`
}

func signature(r protocol.Response) responseSignature {
	sum := sha256.Sum256(r.Body)
	return responseSignature{r.Status, r.Headers.Get("Content-Type"), hex.EncodeToString(sum[:])}
}
func approved(a, b capture) (string, error) {
	if a.viewer != "teen" && a.viewer != "child" {
		return "", nil
	}
	var entries []approvedDifference
	if err := json.Unmarshal(approvedSearch, &entries); err != nil {
		return "", fmt.Errorf("invalid approved differences: %w", err)
	}
	for _, entry := range entries {
		if entry.Viewer == a.viewer && entry.Client == a.client && entry.JSON == a.json && entry.Path == a.path && entry.Baseline == signature(a.response) && entry.Candidate == signature(b.response) {
			if entry.Commit == "" {
				return "c82a7830", nil
			}
			return entry.Commit, nil
		}
	}
	return "", nil
}

// An approved body change can also change Content-Length. All other
// normalized headers must match, and both lengths must describe their body.
func approvedHeaders(a, b capture) bool {
	x, y := normalizeHeaders(a.response.Headers), normalizeHeaders(b.response.Headers)
	for _, item := range []struct {
		h    map[string][]string
		size int
	}{{x, len(a.response.Body)}, {y, len(b.response.Body)}} {
		for _, key := range []string{"Content-Length", "X-Plex-Content-Original-Length"} {
			if values, ok := item.h[key]; ok {
				if len(values) != 1 || values[0] != strconv.Itoa(item.size) {
					return false
				}
				delete(item.h, key)
			}
		}
	}
	return reflect.DeepEqual(x, y)
}
