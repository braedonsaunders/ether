package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	pmsconform "github.com/braedonsaunders/ether/internal/conform"
	conformmb "github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/conform/silo"
)

func TestInfuseRecordedGrouping404IsBlocking(t *testing.T) {
	p, err := conformmb.Load("jellyfin", "../../internal/conform/profiles/jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.LoadCapturedBindings("../../testdata/conform/jellyfin", "infuse-jellyfin"); err != nil {
		t.Fatal(err)
	}
	groupingHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/Me":
			if r.Header.Get("X-Emby-Token") != "grant" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"viewer"}`))
		case "/Items":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[]}`))
		case "/UserViews/GroupingOptions":
			groupingHits++
			if r.URL.Query().Get("userId") != "viewer" || !strings.Contains(r.Header.Get("Authorization"), `Token="grant"`) {
				t.Error("grouping request lost authenticated owner")
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	vars := map[string]string{"user_id": "viewer", "device_id": "device", "access_token": "grant"}
	report, err := compare(p, vars, "../../testdata/conform/jellyfin", server.URL, "grant", "infuse-jellyfin", "/UserViews/GroupingOptions", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if groupingHits != 1 {
		t.Fatalf("grouping request did not reach target exactly once: %d", groupingHits)
	}
	for _, d := range report.Differences {
		if d.Kind == pmsconform.KindStatus && d.Blocking && d.Reference == 200 && d.Target == 404 {
			return
		}
	}
	t.Fatalf("recorded grouping 200 versus target 404 not blocking: %+v", report.Differences)
}

func TestFilterReport(t *testing.T) {
	r := pmsconform.Report{Differences: []pmsconform.Difference{
		{Path: "/MediaContainer/Video/@contentRatingAge", Kind: pmsconform.KindMissing, Blocking: true},
		{Path: "/MediaContainer/Video/@slug", Kind: pmsconform.KindMissing, Blocking: true},
		{Path: "$status", Kind: pmsconform.KindStatus, Blocking: true},
	}}
	got := filterReport(r, regexp.MustCompile(`(@|/)contentRatingAge$|status$`))
	if len(got.Differences) != 2 || got.Differences[0].Path != "/MediaContainer/Video/@slug" || got.Differences[1].Kind != pmsconform.KindStatus {
		t.Fatalf("filtered: %+v", got.Differences)
	}
	// The shipped data-gap list must compile.
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "conform", "plex", "data-gaps.regexp"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := regexp.Compile(strings.TrimSpace(string(b))); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryComparisonRedactsAfterRangeValidation(t *testing.T) {
	p, err := silo.Load("../../internal/conform/profiles/silo")
	if err != nil {
		t.Fatal(err)
	}
	response := pmsconform.Response{Kind: "binary-range", RangeKind: "start", Status: 206, BodyLength: 2, Headers: http.Header{"Content-Type": []string{"video/mp2t"}, "Content-Range": []string{"bytes 0-1/2"}, "Content-Length": []string{"2"}}}
	report := compareRedacted(p, pmsconform.Request{}, response, response, map[string]string{"profile_id": "2"}, "")
	if report.Blocking() != 0 {
		t.Fatalf("valid range was corrupted: %+v", report.Differences)
	}
	target := response
	target.Headers = response.Headers.Clone()
	target.Headers.Set("Content-Type", "video/secret")
	report = compareRedacted(p, pmsconform.Request{}, response, target, map[string]string{"password": "secret"}, "")
	encoded, _ := json.Marshal(report)
	if report.Blocking() == 0 || strings.Contains(string(encoded), "secret") {
		t.Fatalf("mismatch or redaction lost: %s", encoded)
	}
}

func TestPlaylistComparisonRetainsDurationsBeforeRedaction(t *testing.T) {
	p, err := silo.Load("../../internal/conform/profiles/silo")
	if err != nil {
		t.Fatal(err)
	}
	a := pmsconform.Response{Status: 200, Headers: http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}}, Body: []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nsegment.ts\n#EXT-X-ENDLIST\n")}
	b := a
	b.Body = []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nsegment.ts?token=secret\n#EXT-X-ENDLIST\n")
	report := compareRedacted(p, pmsconform.Request{}, a, b, map[string]string{"profile_id": "2", "stream_token": "secret"}, "")
	encoded, _ := json.Marshal(report)
	if report.Blocking() != 0 || strings.Contains(string(encoded), "secret") {
		t.Fatalf("playlist comparison: %s", encoded)
	}
}

func TestOnlyReviewedMissingFieldIsInformational(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"notApplicable":[{"endpoint":"/item","path":"$/file_path","reason":"no server filesystem","clientAudit":"optional and unused"}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	rules, err := loadNotApplicable(dir)
	if err != nil {
		t.Fatal(err)
	}
	report := pmsconform.Report{Differences: []pmsconform.Difference{{Endpoint: "/item", Path: "$/file_path", Kind: pmsconform.KindMissing, Blocking: true}, {Endpoint: "/item", Path: "$/file_path", Kind: pmsconform.KindExtra, Blocking: true}, {Endpoint: "/item", Path: "$/other", Kind: pmsconform.KindMissing, Blocking: true}, {Endpoint: "/other", Path: "$/file_path", Kind: pmsconform.KindMissing, Blocking: true}}}
	applyNotApplicable(&report, "/item", rules)
	if report.Differences[0].Blocking || report.Differences[0].Reason == "" || !report.Differences[1].Blocking || !report.Differences[2].Blocking || !report.Differences[3].Blocking {
		t.Fatalf("rules broadened: %+v", report.Differences)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"notApplicable":[{"endpoint":"/item","path":"$/file_path","reason":"x"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadNotApplicable(dir); err == nil {
		t.Fatal("accepted unaudited rule")
	}
}

func TestCorpusFilter(t *testing.T) {
	all := corpus(conformplex.Protocol{}, "web", "")
	dec := corpus(conformplex.Protocol{}, "web", "/transcode/universal/decision")
	if len(dec) == 0 || len(dec) >= len(all) {
		t.Fatalf("decision filter: %d of %d", len(dec), len(all))
	}
	for _, r := range dec {
		if !strings.Contains(r.Path, "/transcode/universal/decision") {
			t.Fatalf("unexpected %s", r.Path)
		}
	}
}

// fakePMS serves a tiny library: sections, one movie listing and a
// MediaContainer for everything else.
func fakePMS(t *testing.T, extra string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml;charset=utf-8")
		w.Header().Set("X-Plex-Protocol", "1.0")
		switch {
		case r.URL.Path == "/library/sections":
			w.Write([]byte(`<MediaContainer size="1"><Directory key="1" type="movie" title="Movies"/></MediaContainer>`))
		case strings.HasSuffix(r.URL.Path, "/all"):
			w.Write([]byte(`<MediaContainer size="1"><Video ratingKey="7" title="A" slug="a"` + extra + `/></MediaContainer>`))
		default:
			w.Write([]byte(`<MediaContainer size="0"/>`))
		}
	}))
}

func TestRecordOnlyKeepsDiscoveryAndManifest(t *testing.T) {
	srv := fakePMS(t, "")
	defer srv.Close()
	dir := t.TempDir()
	if _, err := record(conformplex.Protocol{}, nil, srv.URL, "tok", dir, "web", "", 5*time.Second, 2); err != nil {
		t.Fatal(err)
	}
	disc := filepath.Join(dir, "web", "_discovery.json")
	before, err := os.ReadFile(disc)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a manifest note for another endpoint; a partial recording keeps it.
	var m map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	json.Unmarshal(b, &m)
	m["trimmedResponses"] = []string{"web /hubs trimmed to first 50 entries"}
	b, _ = json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644)
	os.WriteFile(disc, append(bytes.TrimSpace(before), '\n'), 0o644)

	if _, err := record(conformplex.Protocol{}, nil, srv.URL, "tok", dir, "web", "/identity", 5*time.Second, 2); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(disc)
	if !bytes.Equal(bytes.TrimSpace(after), bytes.TrimSpace(before)) {
		t.Fatalf("partial recording rewrote discovery:\n%s\n%s", before, after)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "manifest.json"))
	if !strings.Contains(string(b), "web /hubs trimmed") {
		t.Fatalf("manifest lost earlier notes: %s", b)
	}
}

func TestCompareSummaryAgainstFixtures(t *testing.T) {
	ref := fakePMS(t, "")
	defer ref.Close()
	dir := t.TempDir()
	if _, err := record(conformplex.Protocol{}, nil, ref.URL, "tok", dir, "web", "", 5*time.Second, 2); err != nil {
		t.Fatal(err)
	}
	// The target lacks nothing but adds an attribute in listings.
	target := fakePMS(t, ` extra="1"`)
	defer target.Close()
	rep, err := compare(conformplex.Protocol{}, nil, dir, target.URL, "tok", "web", "", 5*time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	emit(&out, rep, "text", true, false)
	s := out.String()
	if !strings.Contains(s, "/MediaContainer/Video/@extra") || !strings.Contains(s, "| **total** |") {
		t.Fatalf("summary:\n%s", s)
	}
	if rep.Blocking() != 2 { // xml and json listing
		t.Fatalf("blocking = %d: %+v", rep.Blocking(), rep.Differences)
	}
	out.Reset()
	emit(&out, rep, "json", true, false)
	var js struct {
		Endpoints []pmsconform.EndpointSummary `json:"endpoints"`
	}
	if err := json.Unmarshal(out.Bytes(), &js); err != nil || len(js.Endpoints) != len(corpus(conformplex.Protocol{}, "web", "")) {
		t.Fatalf("json summary: %v %d", err, len(js.Endpoints))
	}
}

func TestValidateFixture(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(spec, []byte(`{"openapi":"3.0.0","paths":{"/x":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object","required":["Id"]}}}}}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := pmsconform.SaveFixture(dir, "response", pmsconform.Response{Status: 200, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"Id":"one"}`)}); err != nil {
		t.Fatal(err)
	}
	args := []string{"-spec", spec, "-fixture", filepath.Join(dir, "response.json"), "-path", "/x"}
	if err := validateFixture(args); err != nil {
		t.Fatal(err)
	}
	if err := validateFixture(append(args[:len(args)-1], "/missing")); err == nil {
		t.Fatal("undocumented path accepted")
	}
}
func TestRecordRejectsEmptyCorpus(t *testing.T) {
	_, err := record(conformplex.Protocol{}, nil, "http://unused", "", t.TempDir(), "web", "/no-such-route", time.Second, 1)
	if err == nil {
		t.Fatal("empty recording was reported as success")
	}
}
func TestSiloPlaybackFlowsCarrySession(t *testing.T) {
	dir := t.TempDir()
	profile := `{"protocol":"silo","client":"test","flows":{"play_direct":[{"name":"start","method":"POST","path":"/api/v2/playback/start","body":{"playback_attempt_id":"${playback_attempt_id}"}}],"progress_reporting":[{"name":"progress","method":"POST","path":"/api/v2/playback/${play_session_id}/progress"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "silo.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := silo.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var attempt string
	var progress bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v2/playback/start" {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			attempt = body["playback_attempt_id"]
			w.Write([]byte(`{"outcome":"playable","session_id":"private-session-123"}`))
			return
		}
		if r.URL.Path == "/api/v2/playback/private-session-123/progress" {
			progress = true
			w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	out := t.TempDir()
	report, err := record(p, map[string]string{}, srv.URL, "", out, "silo", "", 3*time.Second, 2)
	if err != nil || report.Blocking() != 0 {
		t.Fatalf("record: %+v %v", report, err)
	}
	if len(attempt) != 36 || !progress {
		t.Fatalf("attempt or propagated session missing")
	}
	for _, r := range p.Corpus("silo") {
		f := filepath.Join(out, "silo", p.FixtureName("silo", r, true)+".json")
		fixture, err := pmsconform.LoadFixture(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(fixture.Body, []byte("private-session-123")) {
			t.Fatal("session leaked to fixture")
		}
	}
}
