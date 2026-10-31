package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/newznab"
)

// prowlarrTestApp builds an App backed by a throwaway config store and a
// fake Prowlarr whose indexer list body can be swapped between calls.
func prowlarrTestApp(t *testing.T, body *string, status *int, retryAfter string) (*App, *config.Store, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		if *status != http.StatusOK {
			w.WriteHeader(*status)
			return
		}
		fmt.Fprint(w, *body)
	}))
	t.Cleanup(srv.Close)
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &App{cfg: store, log: quietLog()}, store, srv.URL
}

func postProwlarr(t *testing.T, a *App, url string, doImport bool) *httptest.ResponseRecorder {
	t.Helper()
	payload := fmt.Sprintf(`{"config":{"url":%q,"apiKey":"k","dailyLimit":0,"dailyGrabs":0,"priority":0},"import":%v}`, url, doImport)
	req := httptest.NewRequest(http.MethodPost, "/prowlarr", strings.NewReader(payload))
	w := httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(w, req)
	return w
}

func storedIDs(store *config.Store) []string {
	ids := make([]string, 0, len(store.Get().Indexers))
	for _, x := range store.Get().Indexers {
		ids = append(ids, x.ID)
	}
	return ids
}

// Preview reports skipped unknown-protocol indexers and warnings, saves
// nothing, and the response carries no removal field.
func TestProwlarrPreviewReportsSkippedAndWarnings(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":5,"name":"Odd","enable":true,"protocol":"rss"}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	w := postProwlarr(t, a, url, false)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Indexers               []config.Indexer `json:"indexers"`
		Imported               bool             `json:"imported"`
		SkippedUnknownProtocol int              `json:"skippedUnknownProtocol"`
		Warnings               []string         `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Imported || len(out.Indexers) != 1 || out.SkippedUnknownProtocol != 1 {
		t.Fatalf("preview %+v", out)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "unsupported protocol") {
		t.Fatalf("warnings %+v", out.Warnings)
	}
	if strings.Contains(w.Body.String(), `"removed"`) {
		t.Fatalf("response must not report removals: %s", w.Body.String())
	}
	if got := storedIDs(store); len(got) != 0 {
		t.Fatalf("preview saved indexers %v", got)
	}
}

// Import is additive (upsert only): importing twice, with the second
// discovery no longer reporting an indexer, must NOT remove it — nor any
// other stored indexer (manual or previously imported). This locks the
// owner's decision against implicit deletion on import.
func TestProwlarrImportIsAdditiveNeverRemovesAbsent(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":5,"name":"Odd","enable":true,"protocol":"rss"}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	// Pre-existing entries: a manual one and a same-scope import from an
	// earlier run that this discovery does not report at all.
	prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: url})
	if prefix == "" {
		t.Fatal("empty prefix")
	}
	if err := store.Update(func(c *config.Config) error {
		c.Indexers = append(c.Indexers,
			config.Indexer{ID: "manual-1", Name: "manual"},
			config.Indexer{ID: prefix + "99", Name: "previously imported, now absent"},
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	w := postProwlarr(t, a, url, true)
	if w.Code != http.StatusOK {
		t.Fatalf("import1 code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"removed"`) {
		t.Fatalf("import must not report removals: %s", w.Body.String())
	}
	want := []string{"manual-1", prefix + "99", prefix + "4"}
	if got := storedIDs(store); !slices.Equal(got, want) {
		t.Fatalf("after import1 ids=%v want %v", got, want)
	}

	// Second import: indexer 4 vanished from the source; only 7 remains.
	// Nothing stored may be deleted.
	body = `[{"id":7,"name":"Other","enable":true,"protocol":"torrent"}]`
	w = postProwlarr(t, a, url, true)
	if w.Code != http.StatusOK {
		t.Fatalf("import2 code=%d body=%s", w.Code, w.Body.String())
	}
	want = []string{"manual-1", prefix + "99", prefix + "4", prefix + "7"}
	if got := storedIDs(store); !slices.Equal(got, want) {
		t.Fatalf("after import2 ids=%v want %v (import must be additive)", got, want)
	}

	// Third import with an empty discovery also removes nothing.
	body = `[]`
	w = postProwlarr(t, a, url, true)
	if w.Code != http.StatusOK {
		t.Fatalf("import3 code=%d body=%s", w.Code, w.Body.String())
	}
	if got := storedIDs(store); !slices.Equal(got, want) {
		t.Fatalf("after empty import ids=%v want %v", got, want)
	}
}

// A 429 from Prowlarr propagates a validated Retry-After to the caller;
// malformed/huge values are never forwarded verbatim.
func TestProwlarrHandlerPropagatesValidatedRetryAfter(t *testing.T) {
	body := ``
	status := http.StatusTooManyRequests
	a, _, url := prowlarrTestApp(t, &body, &status, "120")
	w := postProwlarr(t, a, url, false)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "120" {
		t.Fatalf("Retry-After=%q want 120", got)
	}

	a2, _, url2 := prowlarrTestApp(t, &body, &status, "soon")
	w = postProwlarr(t, a2, url2, false)
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("malformed Retry-After forwarded verbatim: %q", got)
	}

	a3, _, url3 := prowlarrTestApp(t, &body, &status, "999999999")
	w = postProwlarr(t, a3, url3, false)
	if got := w.Header().Get("Retry-After"); got != "86400" {
		t.Fatalf("huge Retry-After=%q want capped 86400", got)
	}
}

func postProwlarrConfig(t *testing.T, a *App, cfg config.Prowlarr, doImport bool) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"config": cfg, "import": doImport})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/prowlarr", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(w, req)
	return w
}

func enabledIDs(store *config.Store) []string {
	var ids []string
	for _, x := range store.Get().Indexers {
		if x.Enabled {
			ids = append(ids, x.ID)
		}
	}
	return ids
}

// Chosen tags keep only indexers that carry one of them. Losing the tag,
// or disappearing from Prowlarr, disables that indexer so it is not used.
// A manual indexer and a later re-tag are picked up without deleting rows.
func TestProwlarrTagSyncStopsUntaggedAndRemoved(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet","tags":[1]},{"id":5,"name":"Other","enable":true,"protocol":"usenet","tags":[2]}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: url})
	if err := store.Update(func(c *config.Config) error {
		c.Indexers = append(c.Indexers, config.Indexer{ID: "manual-1", Name: "manual", Enabled: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Prowlarr{URL: url, APIKey: "k", Tags: []int{1}}
	w := postProwlarrConfig(t, a, cfg, true)
	if w.Code != http.StatusOK {
		t.Fatalf("import code=%d body=%s", w.Code, w.Body.String())
	}
	if got := enabledIDs(store); !slices.Equal(got, []string{"manual-1", prefix + "4"}) {
		t.Fatalf("enabled=%v", got)
	}
	if saved := store.Get().Prowlarr.Tags; !slices.Equal(saved, []int{1}) || store.Get().Prowlarr.APIKey != "k" {
		t.Fatalf("saved connection %+v", store.Get().Prowlarr)
	}

	body = `[{"id":9,"name":"Fresh","enable":true,"protocol":"torrent","tags":[1]}]`
	w = postProwlarrConfig(t, a, cfg, true)
	if w.Code != http.StatusOK {
		t.Fatalf("resync code=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Stopped int `json:"stopped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Stopped != 1 {
		t.Fatalf("stopped=%d body=%s", out.Stopped, w.Body.String())
	}
	if got := enabledIDs(store); !slices.Equal(got, []string{"manual-1", prefix + "9"}) {
		t.Fatalf("after removal enabled=%v all=%v", got, storedIDs(store))
	}
	disabled := false
	for _, x := range store.Get().Indexers {
		if x.ID == prefix+"4" {
			disabled = !x.Enabled
		}
	}
	if !disabled {
		t.Fatal("indexer that lost the tag is still enabled")
	}

	body = `[{"id":4,"name":"News","enable":true,"protocol":"usenet","tags":[1]},{"id":9,"name":"Fresh","enable":true,"protocol":"torrent","tags":[1]}]`
	w = postProwlarrConfig(t, a, cfg, true)
	if w.Code != http.StatusOK {
		t.Fatalf("retag code=%d body=%s", w.Code, w.Body.String())
	}
	if got := enabledIDs(store); !slices.Equal(got, []string{"manual-1", prefix + "4", prefix + "9"}) {
		t.Fatalf("after retag enabled=%v", got)
	}
}

// An empty tag list is today's import: new indexers are added and ones
// that vanish from Prowlarr stay stored and enabled.
func TestProwlarrEmptyTagListStaysAdditive(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet","tags":[1]},{"id":5,"name":"Other","enable":true,"protocol":"usenet","tags":[2]}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	cfg := config.Prowlarr{URL: url, APIKey: "k"}
	raw := fmt.Sprintf(`{"config":{"url":%q,"apiKey":"k","dailyLimit":0,"dailyGrabs":0,"priority":0,"tags":[]},"import":true}`, url)
	req := httptest.NewRequest(http.MethodPost, "/prowlarr", strings.NewReader(raw))
	w := httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	prefix := newznab.ProwlarrIDPrefix(cfg)
	want := []string{prefix + "4", prefix + "5"}
	if got := enabledIDs(store); !slices.Equal(got, want) {
		t.Fatalf("enabled=%v", got)
	}
	body = `[{"id":5,"name":"Other","enable":true,"protocol":"usenet","tags":[2]}]`
	w = httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/prowlarr", strings.NewReader(raw)))
	if w.Code != http.StatusOK {
		t.Fatalf("second code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"removed"`) || strings.Contains(w.Body.String(), `"stopped"`) {
		t.Fatalf("empty tags must not stop indexers: %s", w.Body.String())
	}
	if got := enabledIDs(store); !slices.Equal(got, want) {
		t.Fatalf("after disappearance enabled=%v", got)
	}
	if len(store.Get().Prowlarr.Tags) != 0 {
		t.Fatalf("tags stored: %v", store.Get().Prowlarr.Tags)
	}
}

// When Prowlarr cannot be read, the last synced indexers stay and the
// response says so. A first failure, with nothing saved, is still an error.
func TestProwlarrUnreachableKeepsLastSetAndShowsStatus(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet","tags":[1]}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	cfg := config.Prowlarr{URL: url, APIKey: "k", Tags: []int{1}}
	w := postProwlarrConfig(t, a, cfg, true)
	if w.Code != http.StatusOK {
		t.Fatalf("import code=%d body=%s", w.Code, w.Body.String())
	}
	before := storedIDs(store)
	status = http.StatusInternalServerError
	w = postProwlarrConfig(t, a, cfg, true)
	if w.Code != http.StatusOK {
		t.Fatalf("unreachable code=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Status   string `json:"status"`
		Kept     bool   `json:"kept"`
		Imported bool   `json:"imported"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "unreachable" || !out.Kept || out.Imported {
		t.Fatalf("status response %+v", out)
	}
	if got := storedIDs(store); !slices.Equal(got, before) {
		t.Fatalf("indexers changed %v -> %v", before, got)
	}
	if !store.Get().Indexers[0].Enabled {
		t.Fatal("last indexer was disabled")
	}
	req := httptest.NewRequest(http.MethodGet, "/prowlarr", nil)
	gw := httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(gw, req)
	if gw.Code != http.StatusOK || !strings.Contains(gw.Body.String(), `"status":"unreachable"`) || !strings.Contains(gw.Body.String(), `"kept":true`) {
		t.Fatalf("GET %d %s", gw.Code, gw.Body.String())
	}
	if strings.Contains(gw.Body.String(), `"apiKey":"k"`) {
		t.Fatalf("API key leaked: %s", gw.Body.String())
	}

	// Nothing saved yet: a failure must not pretend there is a set to keep.
	body = ``
	status = http.StatusInternalServerError
	a2, store2, url2 := prowlarrTestApp(t, &body, &status, "")
	w = postProwlarrConfig(t, a2, config.Prowlarr{URL: url2, APIKey: "k", Tags: []int{1}}, true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("first failure code=%d body=%s", w.Code, w.Body.String())
	}
	if len(store2.Get().Indexers) != 0 || store2.Get().Prowlarr.APIKey != "" {
		t.Fatalf("first failure stored %+v", store2.Get().Prowlarr)
	}
}

func TestProwlarrPeriodicSyncUsesTagsAndKeepsLastSet(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet","tags":[1]}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	if err := store.Update(func(c *config.Config) error {
		c.Prowlarr = config.Prowlarr{URL: url, APIKey: "k"}
		c.Indexers = []config.Indexer{{ID: "manual-1", Name: "manual", Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// No tags: periodic sync does not import.
	a.syncSavedProwlarr(context.Background())
	if got := storedIDs(store); !slices.Equal(got, []string{"manual-1"}) {
		t.Fatalf("untagged periodic changed indexers %v", got)
	}

	if err := store.Update(func(c *config.Config) error {
		c.Prowlarr.Tags = []int{1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.syncSavedProwlarr(context.Background())
	prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: url})
	if got := enabledIDs(store); !slices.Equal(got, []string{"manual-1", prefix + "4"}) {
		t.Fatalf("periodic enabled=%v", got)
	}
	status = http.StatusBadGateway
	a.syncSavedProwlarr(context.Background())
	if got := enabledIDs(store); !slices.Equal(got, []string{"manual-1", prefix + "4"}) {
		t.Fatalf("unreachable periodic enabled=%v", got)
	}
	state, kept := a.prowlarrStatus()
	if state != "unreachable" || !kept {
		t.Fatalf("status=%q kept=%v", state, kept)
	}
}

func TestProwlarrTagsAcceptStoredKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tag" || r.Header.Get("X-Api-Key") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `[{"id":7,"label":"ether"}]`)
	}))
	defer srv.Close()
	store, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error {
		c.Prowlarr = config.Prowlarr{URL: srv.URL, APIKey: "secret", Tags: []int{7}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: store, log: quietLog()}
	payload := fmt.Sprintf(`{"config":{"url":%q,"apiKey":%q}}`, srv.URL, admin.RedactedSecret)
	req := httptest.NewRequest(http.MethodPost, "/prowlarr/tags", strings.NewReader(payload))
	w := httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ether"`) {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRunProwlarrSyncStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&App{}).runProwlarrSync(ctx)
}

func TestProwlarrRetryAfterHeaderValidation(t *testing.T) {
	if got := prowlarrRetryAfterHeader(""); got != "" {
		t.Fatalf("empty=%q", got)
	}
	if got := prowlarrRetryAfterHeader("45"); got != "45" {
		t.Fatalf("45=%q", got)
	}
	if got := prowlarrRetryAfterHeader("999999999"); got != "86400" {
		t.Fatalf("huge=%q want 86400", got)
	}
}
