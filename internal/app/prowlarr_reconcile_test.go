package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/newznab"
)

func prowlarrIDs(in []config.Indexer) []string {
	ids := make([]string, 0, len(in))
	for _, x := range in {
		ids = append(ids, x.ID)
	}
	return ids
}

// Re-import from the same Prowlarr instance removes indexers that vanished
// or were disabled at the source, keeps manual/other-scope indexers, and
// upserts the discovered ones.
func TestProwlarrReconcileRemovesStaleSameScopeOnly(t *testing.T) {
	const prefix = "prowlarr-abc123-"
	existing := []config.Indexer{
		{ID: prefix + "1", Name: "old name"},
		{ID: prefix + "2", Name: "removed at source"},
		{ID: prefix + "3", Name: "disabled at source"},
		{ID: "manual-1", Name: "manual"},
		{ID: "prowlarr-other9-2", Name: "other prowlarr instance"},
		{ID: "cardigann-x", Name: "other origin"},
	}
	// Discovery succeeded; only 1 (renamed) and new 9 come back. 2/3 are gone.
	discovered := []config.Indexer{
		{ID: prefix + "1", Name: "new name"},
		{ID: prefix + "9", Name: "new at source"},
	}
	got, removed, stale := reconcileProwlarrIndexers(existing, discovered, prefix, []string{prefix + "1", prefix + "9"})
	if stale != 0 {
		t.Fatalf("stale=%d want 0", stale)
	}
	if removed != 2 {
		t.Fatalf("removed=%d want 2, got %v", removed, prowlarrIDs(got))
	}
	want := []string{prefix + "1", "manual-1", "prowlarr-other9-2", "cardigann-x", prefix + "9"}
	if !slices.Equal(prowlarrIDs(got), want) {
		t.Fatalf("ids=%v want %v", prowlarrIDs(got), want)
	}
	if got[0].Name != "new name" {
		t.Fatalf("upsert did not refresh: %+v", got[0])
	}
}

// A failed/empty discovery must never be read as "the source is empty":
// nothing is removed, and an empty prefix (unusable URL) also removes
// nothing. The handler additionally aborts on any discovery error before
// reconciling, so this is defence in depth.
func TestProwlarrReconcileFailedOrEmptyDiscoveryRemovesNothing(t *testing.T) {
	const prefix = "prowlarr-abc123-"
	existing := []config.Indexer{
		{ID: prefix + "1"},
		{ID: prefix + "2"},
		{ID: "manual-1"},
	}
	for _, tc := range []struct {
		name       string
		discovered []config.Indexer
		prefix     string
	}{
		{"empty discovery", nil, prefix},
		{"empty prefix", []config.Indexer{{ID: prefix + "1"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, removed, _ := reconcileProwlarrIndexers(slices.Clone(existing), tc.discovered, tc.prefix, nil)
			if removed != 0 {
				t.Fatalf("removed=%d want 0 (%v)", removed, prowlarrIDs(got))
			}
			for _, id := range []string{prefix + "1", prefix + "2", "manual-1"} {
				if !slices.Contains(prowlarrIDs(got), id) {
					t.Fatalf("%s missing from %v", id, prowlarrIDs(got))
				}
			}
		})
	}
}

// Sync decision: an indexer still present at the source but filtered out
// of the importable list (unknown protocol) is non-importable, so its local
// copy IS removed on a discovery treated as authoritative — PresentIDs does not protect
// it; the discovery warning/skipped count explains the removal. A truly
// absent one is also removed. The caller's slice is never mutated/aliased.
func TestProwlarrReconcileRemovesPresentUnknownProtocolAndDoesNotAlias(t *testing.T) {
	const prefix = "prowlarr-abc123-"
	existing := []config.Indexer{
		{ID: prefix + "1", Name: "old name"},
		{ID: prefix + "5", Name: "present but unknown protocol"},
		{ID: prefix + "2", Name: "gone at source"},
		{ID: "manual-1", Name: "manual"},
	}
	discovered := []config.Indexer{{ID: prefix + "1", Name: "new name"}}
	present := []string{prefix + "1", prefix + "5"}
	got, removed, stale := reconcileProwlarrIndexers(existing, discovered, prefix, present)
	if removed != 2 || stale != 0 {
		t.Fatalf("removed=%d stale=%d want 2/0 (%v)", removed, stale, prowlarrIDs(got))
	}
	want := []string{prefix + "1", "manual-1"}
	if !slices.Equal(prowlarrIDs(got), want) {
		t.Fatalf("ids=%v want %v", prowlarrIDs(got), want)
	}
	if existing[0].Name != "old name" {
		t.Fatalf("caller slice mutated via alias: %+v", existing[0])
	}
	// Also without the removal branch (empty prefix): an upsert must not
	// write through into the caller's backing array.
	caller := []config.Indexer{{ID: prefix + "1", Name: "old name"}}
	if _, _, _ = reconcileProwlarrIndexers(caller, discovered, "", present); caller[0].Name != "old name" {
		t.Fatalf("caller slice mutated via alias (no-removal path): %+v", caller[0])
	}
}

// Legacy migration (A): a pre-normalisation orphan whose stored endpoint
// normalises to this instance (EXAMPLE.com:80 -> example.com, default port
// dropped) and whose numeric suffix is in this import is removed and not
// duplicated; the same number on a provably different instance is kept
// silently; an unprovable one is kept and counted as stale.
func TestProwlarrReconcileRemovesPreNormalisationOrphanSameEndpoint(t *testing.T) {
	prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: "http://example.com/base"})
	if prefix == "" {
		t.Fatal("empty prefix")
	}
	existing := []config.Indexer{
		{ID: "prowlarr-oldscope00-4", Name: "orphan", URL: "http://EXAMPLE.com:80/base/4"},
		{ID: "prowlarr-otherscope-4", Name: "other instance", URL: "http://other.example.com:9696/base/4"},
		{ID: "prowlarr-brokenscop-4", Name: "unprovable", URL: ""},
		{ID: "manual-1", Name: "manual"},
	}
	discovered := []config.Indexer{{ID: prefix + "4", Name: "new", URL: "http://example.com/base/4"}}
	got, removed, stale := reconcileProwlarrIndexers(existing, discovered, prefix, []string{prefix + "4"})
	if removed != 1 {
		t.Fatalf("removed=%d want 1 (only proven orphan) ids=%v", removed, prowlarrIDs(got))
	}
	if stale != 1 {
		t.Fatalf("stale=%d want 1 (only unprovable)", stale)
	}
	want := []string{"prowlarr-otherscope-4", "prowlarr-brokenscop-4", "manual-1", prefix + "4"}
	if !slices.Equal(prowlarrIDs(got), want) {
		t.Fatalf("ids=%v want %v", prowlarrIDs(got), want)
	}
	n := 0
	for _, id := range prowlarrIDs(got) {
		if id == prefix+"4" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("discovered indexer duplicated: %v", prowlarrIDs(got))
	}
}

// Composite guard (C2/B): discovered empty but presentIDs non-empty is
// still treated as authoritative (not proven complete) — present-but-unknown
// and gone copies are removed. (Empty+empty removes nothing: covered above.)
func TestProwlarrReconcileEmptyDiscoveredNonEmptyPresentRemoves(t *testing.T) {
	const prefix = "prowlarr-abc123-"
	existing := []config.Indexer{
		{ID: prefix + "5", Name: "present but unknown protocol"},
		{ID: prefix + "2", Name: "gone at source"},
		{ID: "manual-1", Name: "manual"},
	}
	got, removed, stale := reconcileProwlarrIndexers(existing, nil, prefix, []string{prefix + "5"})
	if removed != 2 || stale != 0 {
		t.Fatalf("removed=%d stale=%d want 2/0 (%v)", removed, stale, prowlarrIDs(got))
	}
	if want := []string{"manual-1"}; !slices.Equal(prowlarrIDs(got), want) {
		t.Fatalf("ids=%v want %v", prowlarrIDs(got), want)
	}
}

// --- Handler (real HTTP through playbackPolicyHandler) ---

func prowlarrHandlerApp(t *testing.T, upstream string) *App {
	t.Helper()
	cfg, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Indexers = []config.Indexer{{ID: "manual-1", Name: "manual"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = upstream
	return &App{cfg: cfg, log: quietLog()}
}

func postProwlarr(t *testing.T, a *App, url string, importIt bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"config": map[string]any{"url": url, "apiKey": "k"},
		"import": importIt,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/prowlarr", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(rec, req)
	return rec
}

func indexerIDsOf(a *App) []string { return prowlarrIDs(a.cfg.Get().Indexers) }

// 429: a valid Retry-After reaches the caller and the config is untouched
// (no import, no removal); absent/malformed/past values are not forwarded;
// a huge value is capped, not forwarded verbatim.
func TestProwlarrHandler429RetryAfterValidatedAndConfigUntouched(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		name, header, want string
		set                bool
	}{
		{name: "valid", header: "120", want: "120", set: true},
		{name: "absent", header: "", want: "", set: false},
		{name: "malformed", header: "soon", want: "", set: true},
		{name: "past date", header: past, want: "", set: true},
		{name: "huge capped", header: "999999999", want: "86400", set: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.set {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()
			a := prowlarrHandlerApp(t, srv.URL)
			before := indexerIDsOf(a)
			rec := postProwlarr(t, a, srv.URL, true)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("code=%d want 502, body=%q", rec.Code, rec.Body.String())
			}
			got, present := rec.Header()["Retry-After"]
			if tc.want == "" && present {
				t.Fatalf("Retry-After forwarded %q, want none (upstream %q)", got, tc.header)
			}
			if tc.want != "" && (!present || len(got) != 1 || got[0] != tc.want) {
				t.Fatalf("Retry-After=%q want [%q]", got, tc.want)
			}
			if after := indexerIDsOf(a); !slices.Equal(after, before) {
				t.Fatalf("config mutated on 429: %v -> %v", before, after)
			}
		})
	}
}

// A valid future HTTP-date Retry-After is propagated (as seconds, within
// the remaining window) — not dropped like a past/malformed value.
func TestProwlarrHandler429FutureHTTPDateRetryAfter(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", future)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	a := prowlarrHandlerApp(t, srv.URL)
	before := indexerIDsOf(a)
	rec := postProwlarr(t, a, srv.URL, true)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502, body=%q", rec.Code, rec.Body.String())
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs <= 7000 || secs > 7200 {
		t.Fatalf("Retry-After=%q want seconds in (7000,7200] for %q", rec.Header().Get("Retry-After"), future)
	}
	if after := indexerIDsOf(a); !slices.Equal(after, before) {
		t.Fatalf("config mutated on 429: %v -> %v", before, after)
	}
}

// Direct unit test of the handler helper: it must itself accept a valid
// future HTTP-date (the value it receives is not always pre-canonicalised
// seconds), and reject past/malformed values.
func TestProwlarrRetryAfterHeaderFutureHTTPDate(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	secs, err := strconv.Atoi(prowlarrRetryAfterHeader(future))
	if err != nil || secs <= 7000 || secs > 7200 {
		t.Fatalf("prowlarrRetryAfterHeader(%q)=%d,%v want seconds in (7000,7200]", future, secs, err)
	}
	if got := prowlarrRetryAfterHeader(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)); got != "" {
		t.Fatalf("past date = %q, want empty", got)
	}
}

// An unprovable pre-normalisation orphan is kept on import and reported
// countably: staleProwlarrIndexers plus the named warning.
func TestProwlarrHandlerImportReportsStaleLegacyIndexer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":4,"name":"News","enable":true,"protocol":"usenet"}]`)
	}))
	defer srv.Close()
	a := prowlarrHandlerApp(t, srv.URL)
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Indexers = append(c.Indexers, config.Indexer{ID: "prowlarr-brokenscop-4", Name: "unprovable", URL: ""})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := postProwlarr(t, a, srv.URL, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	var out struct {
		Removed               int      `json:"removed"`
		StaleProwlarrIndexers int      `json:"staleProwlarrIndexers"`
		Warnings              []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.StaleProwlarrIndexers != 1 || out.Removed != 0 {
		t.Fatalf("report %+v", out)
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "stale prowlarr indexer from previous import spelling") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings %v lack stale-legacy warning", out.Warnings)
	}
	if ids := indexerIDsOf(a); !slices.Contains(ids, "prowlarr-brokenscop-4") {
		t.Fatalf("unprovable orphan removed: %v", ids)
	}
}

// Preview (import=false) never persists; import=true does (positive
// control, so the preview assertion is not vacuous), and an upstream
// indexer filtered for unknown protocol is reported, not imported.
func TestProwlarrHandlerPreviewDoesNotPersist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":5,"name":"Odd","enable":true,"protocol":"rss"}]`)
	}))
	defer srv.Close()
	a := prowlarrHandlerApp(t, srv.URL)
	before := indexerIDsOf(a)

	rec := postProwlarr(t, a, srv.URL, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview code=%d body=%q", rec.Code, rec.Body.String())
	}
	var out struct {
		Imported               bool             `json:"imported"`
		Removed                int              `json:"removed"`
		SkippedUnknownProtocol int              `json:"skippedUnknownProtocol"`
		Warnings               []string         `json:"warnings"`
		Indexers               []config.Indexer `json:"indexers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Imported || out.Removed != 0 || out.SkippedUnknownProtocol != 1 || len(out.Warnings) != 1 || len(out.Indexers) != 1 {
		t.Fatalf("preview report %+v", out)
	}
	if after := indexerIDsOf(a); !slices.Equal(after, before) {
		t.Fatalf("preview persisted: %v -> %v", before, after)
	}

	rec = postProwlarr(t, a, srv.URL, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("import code=%d body=%q", rec.Code, rec.Body.String())
	}
	after := indexerIDsOf(a)
	wantPrefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: srv.URL})
	if len(after) != 2 || after[0] != "manual-1" || after[1] != wantPrefix+"4" {
		t.Fatalf("import did not persist discovered indexer: %v (prefix %q)", after, wantPrefix)
	}
}
