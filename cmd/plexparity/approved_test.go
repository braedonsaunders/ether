package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	"net/http"
	"reflect"
	"strings"
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
	if len(entries) != 940 {
		t.Fatalf("got %d scoped response pairs, want 940", len(entries))
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

func TestApprovedResumeRegistryIsExact(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedResume, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 9 {
		t.Fatalf("got %d resume response pairs, want 9", len(entries))
	}
	allowed := map[string]bool{
		"playback:infuse-resume:61000:metadata":  true,
		"playback:infuse-resume:120000:metadata": true,
		"playback:infuse-resume:201274:metadata": true,
		"playback:infuse-resume:61000:continue":  true,
		"playback:infuse-resume:120000:continue": true,
		"playback:infuse-resume:201274:continue": true,
	}
	for _, e := range entries {
		if !allowed[e.Path] || e.Viewer != "owner" || e.Client != "appletv" || !e.JSON || e.Commit != "1d5a1ff6" || len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("resume approval is not an exact reviewed pair: %+v", e)
		}
	}
}

func TestApprovedDateAddedRegistryIsExact(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedDateAdded, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2873 {
		t.Fatalf("got %d unique date response pairs, want 2873", len(entries))
	}
	viewers := map[string]bool{"owner": true, "adult": true, "teen": true, "child": true, "friend": true}
	clients := map[string]bool{"web": true, "ios": true, "appletv": true, "androidtv": true, "desktop": true}
	seen := make(map[approvedDifference]bool, len(entries))
	for _, e := range entries {
		if seen[e] || !viewers[e.Viewer] || !clients[e.Client] || e.Path == "" || e.Commit != "77af603e" || e.Baseline.Status != 200 || e.Candidate.Status != 200 || e.Baseline.ContentType != e.Candidate.ContentType || len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("date approval is not a unique exact reviewed pair: %+v", e)
		}
		seen[e] = true
	}
	old := approvedDateAdded
	defer func() { approvedDateAdded = old }()
	a := capture{viewer: "owner", client: "web", path: "/date-test", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte("before")}}
	b := a
	b.response.Body = []byte("after")
	entry := approvedDifference{Viewer: a.viewer, Client: a.client, Path: a.path, Baseline: signature(a.response), Candidate: signature(b.response), Commit: "77af603e"}
	approvedDateAdded, _ = json.Marshal([]approvedDifference{entry})
	if got, err := approved(a, b); err != nil || got != entry.Commit {
		t.Fatalf("exact date pair: %q %v", got, err)
	}
	b.response.Body = []byte("unreviewed")
	if got, err := approved(a, b); err != nil || got != "" {
		t.Fatalf("unreviewed body approved: %q %v", got, err)
	}
	b.response.Body = []byte("after")
	a.viewer, b.viewer = "friend", "friend"
	if got, err := approved(a, b); err != nil || got != "" {
		t.Fatalf("unreviewed viewer approved: %q %v", got, err)
	}
}

func TestApprovedCompositeRegistryIsExact(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedComposite, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 86 {
		t.Fatalf("got %d composite pairs, want 86", len(entries))
	}
	want := map[string]int{"c82a7830+77af603e": 60, "2c7bcdc9+77af603e": 20, "1d5a1ff6+77af603e": 6}
	seen := make(map[approvedDifference]bool, len(entries))
	counts := make(map[string]int)
	for _, e := range entries {
		if seen[e] || e.Path == "" || e.Viewer == "" || e.Client == "" || e.Baseline.Status != 200 || e.Candidate.Status != 200 || e.Baseline.ContentType != e.Candidate.ContentType || len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("composite approval is not an exact unique pair: %+v", e)
		}
		if e.Commit != "1d5a1ff6+77af603e" && e.Viewer != "teen" && e.Viewer != "child" {
			t.Fatalf("policy approval includes unrestricted viewer: %+v", e)
		}
		if e.Commit == "1d5a1ff6+77af603e" && (e.Viewer != "owner" || e.Client != "appletv") {
			t.Fatalf("unexpected resume viewer/client: %+v", e)
		}
		seen[e] = true
		counts[e.Commit]++
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("composite provenance counts %v, want %v", counts, want)
	}
	old := approvedComposite
	defer func() { approvedComposite = old }()
	a := capture{viewer: "teen", client: "web", path: "/composite-test", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte("before")}}
	b := a
	b.response.Body = []byte("after")
	e := approvedDifference{Viewer: a.viewer, Client: a.client, Path: a.path, Baseline: signature(a.response), Candidate: signature(b.response), Commit: "c82a7830+77af603e"}
	approvedComposite, _ = json.Marshal([]approvedDifference{e})
	if got, err := approved(a, b); err != nil || got != e.Commit {
		t.Fatalf("exact composite pair: %q %v", got, err)
	}
	b.response.Body = []byte("unreviewed")
	if got, err := approved(a, b); err != nil || got != "" {
		t.Fatalf("unreviewed composite body approved: %q %v", got, err)
	}
}

func TestApprovedActiveDateRegistryIsExact(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedActiveDate, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2066 {
		t.Fatalf("got %d active date pairs, want 2066", len(entries))
	}
	want := map[string]int{"77af603e": 1986, "c82a7830+77af603e": 60, "2c7bcdc9+77af603e": 20}
	seen := make(map[approvedDifference]bool, len(entries))
	counts := make(map[string]int)
	for _, e := range entries {
		if seen[e] || e.Path == "" || e.Viewer == "" || e.Client == "" || e.Baseline.Status != 200 || e.Candidate.Status != 200 || e.Baseline.ContentType != e.Candidate.ContentType || len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("active date approval is not an exact unique pair: %+v", e)
		}
		if e.Commit != "77af603e" && e.Viewer != "teen" && e.Viewer != "child" {
			t.Fatalf("policy approval includes unrestricted viewer: %+v", e)
		}
		seen[e] = true
		counts[e.Commit]++
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("active date provenance counts %v, want %v", counts, want)
	}
	old := approvedActiveDate
	defer func() { approvedActiveDate = old }()
	a := capture{viewer: "owner", client: "web", path: "/active-date-test", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte("before")}}
	b := a
	b.response.Body = []byte("after")
	e := approvedDifference{Viewer: a.viewer, Client: a.client, Path: a.path, Baseline: signature(a.response), Candidate: signature(b.response), Commit: "77af603e"}
	approvedActiveDate, _ = json.Marshal([]approvedDifference{e})
	if got, err := approved(a, b); err != nil || got != e.Commit {
		t.Fatalf("exact active date pair: %q %v", got, err)
	}
	b.response.Body = []byte("unreviewed")
	if got, err := approved(a, b); err != nil || got != "" {
		t.Fatalf("unreviewed active date body approved: %q %v", got, err)
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

func TestApprovedActiveResumeRegistryIsExact(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedActiveResume, &entries); err != nil {
		t.Fatal(err)
	}
	const baseline = "451ef1e4adce135855260f81a50b43f650908c2d3d963860207cf1c8496e229a"
	want := map[string]string{
		"playback:infuse-resume:61000:continue":  "b10f3fb614c8a26c52ba00edb0d13bb7f808ea3c02f1d722b82f2e080ebbadbf",
		"playback:infuse-resume:120000:continue": "7c684e588d72e6e1f7bf633d426b7e1c413ada67ce734f40a74d39696e399e37",
		"playback:infuse-resume:201274:continue": "8b0f8323b436ffffca080440fc4dfd7c6061abc1deb909c4f1d24b7a59590edb",
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d active resume pairs, want %d", len(entries), len(want))
	}
	for _, e := range entries {
		candidate, ok := want[e.Path]
		if !ok || e.Viewer != "owner" || e.Client != "appletv" || !e.JSON || e.Commit != "1d5a1ff6+77af603e" || e.Baseline != (responseSignature{200, "application/json", baseline}) || e.Candidate != (responseSignature{200, "application/json", candidate}) {
			t.Fatalf("unexpected active resume approval: %+v", e)
		}
		delete(want, e.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing active resume approvals: %v", want)
	}

	// Exercise the ordinary exact matcher with a synthetic pair; real approved
	// bodies remain only in the separately retained evidence logs.
	old := approvedActiveResume
	defer func() { approvedActiveResume = old }()
	a := capture{viewer: "owner", client: "appletv", json: true, path: "playback:infuse-resume:61000:continue", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte("before")}}
	b := a
	b.response.Body = []byte("after")
	e := approvedDifference{Viewer: a.viewer, Client: a.client, JSON: a.json, Path: a.path, Baseline: signature(a.response), Candidate: signature(b.response), Commit: "1d5a1ff6+77af603e"}
	approvedActiveResume, _ = json.Marshal([]approvedDifference{e})
	if got, err := approved(a, b); err != nil || got != e.Commit {
		t.Fatalf("exact active resume pair: %q %v", got, err)
	}
	for _, mutate := range []func(*capture){
		func(c *capture) { c.viewer = "adult" },
		func(c *capture) { c.client = "web" },
		func(c *capture) { c.json = false },
		func(c *capture) { c.path += "?unreviewed=1" },
		func(c *capture) { c.response.Status = 201 },
		func(c *capture) { c.response.Headers = http.Header{"Content-Type": {"text/xml"}} },
	} {
		changed := a
		mutate(&changed)
		if got, err := approved(changed, b); err != nil || got != "" {
			t.Fatalf("unreviewed active resume input approved: %q %v", got, err)
		}
	}
	b.response.Body = []byte("unreviewed")
	if got, err := approved(a, b); err != nil || got != "" {
		t.Fatalf("unreviewed active resume body approved: %q %v", got, err)
	}
}

func TestApprovedActiveSubtitleDateRegistryIsExact(t *testing.T) {
	if got := fmt.Sprintf("%x", sha256.Sum256(approvedActiveSubtitleDate)); got != "95d1c0dce21c46b925f751d809ec8fb9af8d2710d748e078f4a01d0f953a02ae" {
		t.Fatalf("subtitle approval set hash = %s", got)
	}
	var entries []approvedDifference
	if err := json.Unmarshal(approvedActiveSubtitleDate, &entries); err != nil {
		t.Fatal(err)
	}
	viewers := map[string]bool{"owner": true, "adult": true, "teen": true, "child": true, "friend": true}
	clients := map[string]bool{"web": true, "ios": true, "appletv": true, "androidtv": true, "desktop": true}
	seen := make(map[string]bool)
	for _, e := range entries {
		key := e.Viewer + "/" + e.Client
		if seen[key] || !viewers[e.Viewer] || !clients[e.Client] || !e.JSON || e.Path != "subtitle:metadata" || e.Commit != "77af603e" || e.Baseline.Status != 200 || e.Candidate.Status != 200 || e.Baseline.ContentType != "application/json" || e.Candidate.ContentType != "application/json" || len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("unexpected active subtitle approval: %+v", e)
		}
		seen[key] = true
	}
	if len(entries) != 25 || len(seen) != 25 {
		t.Fatalf("got %d entries and %d distinct viewer/client pairs, want 25", len(entries), len(seen))
	}
	old := approvedActiveSubtitleDate
	defer func() { approvedActiveSubtitleDate = old }()
	a := capture{viewer: "owner", client: "web", json: true, path: "subtitle:metadata", response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte("before")}}
	b := a
	b.response.Body = []byte("after")
	e := approvedDifference{Viewer: a.viewer, Client: a.client, JSON: true, Path: a.path, Baseline: signature(a.response), Candidate: signature(b.response), Commit: "77af603e"}
	approvedActiveSubtitleDate, _ = json.Marshal([]approvedDifference{e})
	if got, err := approved(a, b); err != nil || got != e.Commit {
		t.Fatalf("exact subtitle pair: %q %v", got, err)
	}
	b.response.Body = []byte("unreviewed")
	if got, err := approved(a, b); err != nil || got != "" {
		t.Fatalf("unreviewed subtitle body approved: %q %v", got, err)
	}
}

func TestApprovedWatchRegistryIsExact(t *testing.T) {
	var entries []approvedDifference
	if err := json.Unmarshal(approvedWatch, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 129 {
		t.Fatalf("got %d watch response pairs, want 129", len(entries))
	}
	viewers := map[string]bool{"owner": true, "adult": true, "teen": true, "child": true, "friend": true}
	clients := map[string]bool{"web": true, "ios": true, "appletv": true, "androidtv": true, "desktop": true}
	counts := map[string]int{}
	seen := make(map[approvedDifference]bool, len(entries))
	for _, e := range entries {
		kind := e.Path
		if i := strings.Index(kind, "?"); i >= 0 {
			kind = kind[:i]
		}
		switch kind {
		case "sequence:/:/scrobble", "sequence:/:/unscrobble", "/:/eventsource/notifications", "/:/websocket/notifications", "/:/websockets/notifications", "playback:infuse-resume:30000:reset", "playback:infuse-resume:61000:reset", "playback:infuse-resume:120000:reset", "playback:infuse-resume:201274:reset":
		default:
			t.Fatalf("watch approval includes an unreviewed route: %+v", e)
		}
		counts[kind]++
		if seen[e] || !viewers[e.Viewer] || !clients[e.Client] || e.Commit != "23d9fffb2" || e.Baseline.Status == 0 || e.Candidate.Status == 0 || len(e.Baseline.SHA256) != 64 || len(e.Candidate.SHA256) != 64 || e.Baseline.SHA256 == e.Candidate.SHA256 {
			t.Fatalf("watch approval is not a unique exact reviewed pair: %+v", e)
		}
		seen[e] = true
	}
	want := map[string]int{
		"sequence:/:/scrobble": 25, "sequence:/:/unscrobble": 25,
		"/:/eventsource/notifications": 25, "/:/websocket/notifications": 25, "/:/websockets/notifications": 25,
		"playback:infuse-resume:30000:reset": 1, "playback:infuse-resume:61000:reset": 1,
		"playback:infuse-resume:120000:reset": 1, "playback:infuse-resume:201274:reset": 1,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("watch approval counts %v, want %v", counts, want)
	}
	old := approvedWatch
	defer func() { approvedWatch = old }()
	a := capture{viewer: "owner", client: "web", json: true, path: "sequence:/:/scrobble", response: protocol.Response{Status: 200, Body: []byte("")}}
	b := capture{viewer: a.viewer, client: a.client, json: true, path: a.path, response: protocol.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte("{\"MediaContainer\":{\"size\":0}}\n")}}
	e := approvedDifference{Viewer: a.viewer, Client: a.client, JSON: true, Path: a.path, Baseline: signature(a.response), Candidate: signature(b.response), Commit: "23d9fffb2"}
	approvedWatch, _ = json.Marshal([]approvedDifference{e})
	if got, err := approved(a, b); err != nil || got != e.Commit {
		t.Fatalf("exact watch pair: %q %v", got, err)
	}
	b.response.Body = []byte("unreviewed")
	if got, err := approved(a, b); err != nil || got != "" {
		t.Fatalf("unreviewed watch body approved: %q %v", got, err)
	}
}

func TestWatchApprovalAcceptsOnlyTheTypedContainerAndHandshake(t *testing.T) {
	body := []byte("{\"MediaContainer\":{\"size\":0}}\n")
	pair := func(extra string, contentType string) (capture, capture) {
		base := http.Header{"Content-Length": {"0"}, "X-Plex-Protocol": {"1.0"}}
		cand := http.Header{"Content-Length": {fmt.Sprintf("%d", len(body))}, "Content-Type": {contentType}, "Cache-Control": {"no-cache"}, "X-Plex-Protocol": {"1.0"}}
		if extra != "" {
			cand.Set("X-Extra", extra)
		}
		a := capture{path: "sequence:/:/scrobble", response: protocol.Response{Headers: base, Body: []byte("")}}
		b := capture{path: a.path, response: protocol.Response{Headers: cand, Body: body}}
		return a, b
	}
	a, b := pair("", "application/json")
	if !approvedWatchHeaders(a, b) {
		t.Fatal("expected the empty container headers")
	}
	a, b = pair("1", "application/json")
	if approvedWatchHeaders(a, b) {
		t.Fatal("unrelated header difference accepted")
	}
	a, b = pair("", "text/xml")
	if approvedWatchHeaders(a, b) {
		t.Fatal("unreviewed content type accepted")
	}
	socket := func(extra string) (capture, capture) {
		base := make(http.Header)
		cand := make(http.Header)
		base.Set("Sec-WebSocket-Accept", "aaa")
		base.Set("Content-Length", "1")
		cand.Set("Sec-WebSocket-Accept", "bbb")
		cand.Set("Content-Length", "1")
		if extra != "" {
			cand.Set("X-Extra", extra)
		}
		a := capture{path: "/:/websocket/notifications", response: protocol.Response{Headers: base, Body: []byte("a")}}
		b := capture{path: a.path, response: protocol.Response{Headers: cand, Body: []byte("a")}}
		return a, b
	}
	wa, wb := socket("")
	if !approvedWatchHeaders(wa, wb) {
		t.Fatal("expected the handshake token to be ignored")
	}
	wa, wb = socket("1")
	if approvedWatchHeaders(wa, wb) {
		t.Fatal("unrelated websocket header accepted")
	}
}
