package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// c82a7830 deliberately filters restricted viewers before the search limit
// and backfills the visible results. These exact fixture response pairs are
// approved; route/viewer matching alone could hide an unrelated regression.
//
//go:embed approved_search.json
var approvedSearch []byte

// Scope-fix approvals are exact, independently reviewed before/after pairs
// for the PMS-compatible recent-items filters. No route-only approval exists.
//
//go:embed approved_scopes.json
var approvedScopes []byte

// Owner-approved early resume changes, reviewed as exact response pairs.
//
//go:embed approved_resume.json
var approvedResume []byte

// Owner-approved release/air-date change, audited as exact response pairs.
// The fixture uses a uniform release date; ordering coverage is separate.
//
//go:embed approved_date_added.json
var approvedDateAdded []byte

// Exact independently audited compositions of restricted-viewer or resume
// changes with Date Added. Commit records both constituent approvals.
//
//go:embed approved_composite.json
var approvedComposite []byte

// Exact active-model fixture pairs independently audited against the existing
// Date Added and restricted-viewer decisions; no resume pairs are included.
//
//go:embed approved_active_date.json
var approvedActiveDate []byte

// Exact active-model resume and Date Added pairs, independently attributed to
// both approved changes using the controlled retained-model counterfactual.
//
//go:embed approved_active_resume.json
var approvedActiveResume []byte

// Exact subtitle-world active-model pairs, independently audited as the
// existing owner-approved Date Added change.
//
//go:embed approved_active_subtitle_date.json
var approvedActiveSubtitleDate []byte

// Exact scrobble, timeline, and Infuse reset pairs from the 80208da4d versus
// 0901e52df run. Both worlds produced the same 129 pairs.
//
//go:embed approved_watch.json
var approvedWatch []byte

// Exact DVR listing pairs from the 80208da4d versus 0b675e5d6 run. Both
// worlds produced the same 100 pairs. The only body change is refreshedAt.
//
//go:embed approved_livetv.json
var approvedLiveTV []byte

// Exact v0.2.58 versus current prism pairs for the documented release-date,
// subtitle, sidecar, account mode, and restricted-search changes.
//
//go:embed approved_v0258.json
var approvedV0258 []byte

// The same run's playlist and DASH pairs. The candidate hash is of the body
// after the per-response stream grant is replaced with the harness placeholder.
//
//go:embed approved_v0258_token.json
var approvedV0258Token []byte

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
	before, after := signature(a.response), signature(b.response)
	norm := b.response
	norm.Body = normalizeResponse(b.response.Body)
	afterNorm := signature(norm)
	for _, registry := range []struct {
		data               []byte
		restricted         bool
		normalizeCandidate bool
	}{{approvedSearch, true, false}, {approvedScopes, false, false}, {approvedResume, false, false}, {approvedDateAdded, false, false}, {approvedComposite, false, false}, {approvedActiveDate, false, false}, {approvedActiveResume, false, false}, {approvedActiveSubtitleDate, false, false}, {approvedWatch, false, false}, {approvedLiveTV, false, false}, {approvedV0258, false, false}, {approvedV0258Token, false, true}} {
		if registry.restricted && a.viewer != "teen" && a.viewer != "child" {
			continue
		}
		got := after
		if registry.normalizeCandidate {
			got = afterNorm
		}
		var entries []approvedDifference
		if err := json.Unmarshal(registry.data, &entries); err != nil {
			return "", fmt.Errorf("invalid approved differences: %w", err)
		}
		for _, entry := range entries {
			if entry.Viewer == a.viewer && entry.Client == a.client && entry.JSON == a.json && entry.Path == a.path && entry.Baseline == before && entry.Candidate == got {
				if entry.Commit == "" && registry.restricted {
					return "c82a7830", nil
				}
				return entry.Commit, nil
			}
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

// The approved scope fix turns a populated response into a small empty one.
// Ether's compression middleware consequently omits Vary: Accept-Encoding.
// Accept that exact directional header pair only for an otherwise exact
// reviewed scope response; all other headers and valid body lengths still match.
func approvedScopeHeaders(a, b capture) bool {
	x, y := a.response.Headers.Clone(), b.response.Headers.Clone()
	if !reflect.DeepEqual(x.Values("Vary"), []string{"Accept-Encoding"}) || len(y.Values("Vary")) != 0 {
		return false
	}
	x.Del("Vary")
	a.response.Headers, b.response.Headers = x, y
	return approvedHeaders(a, b)
}

// The watched-state fix replaces an empty 200 with an empty MediaContainer.
// That reply gains Content-Type and Cache-Control. A websocket notification
// also carries a per-connection handshake token, which the comparison already
// ignores. Accept those two differences only; every other header still matches.
func approvedWatchHeaders(a, b capture) bool {
	x, y := a.response.Headers.Clone(), b.response.Headers.Clone()
	if strings.HasPrefix(a.path, "/:/websocket") {
		if x.Get("Sec-WebSocket-Accept") == "" || y.Get("Sec-WebSocket-Accept") == "" {
			return false
		}
		x.Del("Sec-WebSocket-Accept")
		y.Del("Sec-WebSocket-Accept")
	}
	if x.Get("Content-Type") == "" && x.Get("Cache-Control") == "" && y.Get("Content-Type") == "application/json" && y.Get("Cache-Control") == "no-cache" {
		y.Del("Content-Type")
		y.Del("Cache-Control")
	}
	a.response.Headers, b.response.Headers = x, y
	return approvedHeaders(a, b)
}
