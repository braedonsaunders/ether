package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

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

func postProwlarrSync(t *testing.T, a *App, url string, doImport bool) *httptest.ResponseRecorder {
	t.Helper()
	payload := fmt.Sprintf(`{"config":{"url":%q,"apiKey":"k","dailyLimit":0,"dailyGrabs":0,"priority":0},"import":%v,"sync":true}`, url, doImport)
	req := httptest.NewRequest(http.MethodPost, "/prowlarr", strings.NewReader(payload))
	w := httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(w, req)
	return w
}

type prowlarrSyncResponse struct {
	Indexers               []config.Indexer `json:"indexers"`
	Imported               bool             `json:"imported"`
	Removed                int              `json:"removed"`
	Removals               []config.Indexer `json:"removals"`
	KeptEdited             []config.Indexer `json:"keptEdited"`
	SkippedUnknownProtocol int              `json:"skippedUnknownProtocol"`
	Warnings               []string         `json:"warnings"`
}

func decodeSync(t *testing.T, w *httptest.ResponseRecorder) prowlarrSyncResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var out prowlarrSyncResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func indexerIDs(indexers []config.Indexer) []string {
	ids := make([]string, 0, len(indexers))
	for _, x := range indexers {
		ids = append(ids, x.ID)
	}
	return ids
}

// seedProwlarrImported imports the current body additively, so the store
// holds exactly the entries discovery generates (unedited identities).
func seedProwlarrImported(t *testing.T, a *App, url string) {
	t.Helper()
	if w := postProwlarr(t, a, url, true); w.Code != http.StatusOK {
		t.Fatalf("seed import code=%d body=%s", w.Code, w.Body.String())
	}
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

// Sync preview lists same-scope entries that vanished or were disabled
// at the source as removals, without removing anything.
func TestProwlarrSyncPreviewListsRemovalsWithoutRemoving(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":99,"name":"Old","enable":true,"protocol":"usenet"}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: url})
	seedProwlarrImported(t, a, url)

	// 99 is now disabled at the source; only 4 remains enabled.
	body = `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":99,"name":"Old","enable":false,"protocol":"usenet"}]`
	out := decodeSync(t, postProwlarrSync(t, a, url, false))
	if out.Imported || out.Removed != 0 {
		t.Fatalf("preview imported=%v removed=%d", out.Imported, out.Removed)
	}
	if got := indexerIDs(out.Removals); !slices.Equal(got, []string{prefix + "99"}) {
		t.Fatalf("removals=%v want [%s]", got, prefix+"99")
	}
	if len(out.KeptEdited) != 0 {
		t.Fatalf("keptEdited=%v want empty", indexerIDs(out.KeptEdited))
	}
	if got, want := storedIDs(store), []string{prefix + "4", prefix + "99"}; !slices.Equal(got, want) {
		t.Fatalf("preview changed store ids=%v want %v", got, want)
	}
	// A plain (non-sync) preview must not compute removals at all.
	if w := postProwlarr(t, a, url, false); strings.Contains(w.Body.String(), `"removals"`) || strings.Contains(w.Body.String(), `"removed"`) {
		t.Fatalf("plain preview reported removals: %s", w.Body.String())
	}
}

// Sync apply removes the listed same-scope entries and upserts the rest;
// without sync=true the same import stays additive.
func TestProwlarrSyncApplyRemovesListedAndNonSyncDoesNot(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":99,"name":"Old","enable":true,"protocol":"usenet"}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: url})
	if err := store.Update(func(c *config.Config) error {
		c.Indexers = append(c.Indexers, config.Indexer{ID: "manual-1", Name: "manual"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seedProwlarrImported(t, a, url)

	body = `[{"id":4,"name":"News","enable":true,"protocol":"usenet"}]`
	// Additive import first: 99 survives.
	seedProwlarrImported(t, a, url)
	if got, want := storedIDs(store), []string{"manual-1", prefix + "4", prefix + "99"}; !slices.Equal(got, want) {
		t.Fatalf("non-sync import ids=%v want %v", got, want)
	}
	// Explicit sync apply: 99 is removed, manual and present entries stay.
	out := decodeSync(t, postProwlarrSync(t, a, url, true))
	if !out.Imported || out.Removed != 1 {
		t.Fatalf("sync apply imported=%v removed=%d", out.Imported, out.Removed)
	}
	if got := indexerIDs(out.Removals); !slices.Equal(got, []string{prefix + "99"}) {
		t.Fatalf("sync removals=%v", got)
	}
	if got, want := storedIDs(store), []string{"manual-1", prefix + "4"}; !slices.Equal(got, want) {
		t.Fatalf("after sync ids=%v want %v", got, want)
	}
}

// An empty or all-disabled source never removes anything on sync, and
// says so with the explicit warning.
func TestProwlarrSyncEmptyOrAllDisabledNeverRemoves(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", `[]`},
		{"all disabled", `[{"id":4,"name":"News","enable":false,"protocol":"usenet"},{"id":99,"name":"Old","enable":false,"protocol":"torrent"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":99,"name":"Old","enable":true,"protocol":"usenet"}]`
			status := http.StatusOK
			a, store, url := prowlarrTestApp(t, &body, &status, "")
			prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: url})
			seedProwlarrImported(t, a, url)
			want := []string{prefix + "4", prefix + "99"}

			body = tc.body
			for _, apply := range []bool{false, true} {
				out := decodeSync(t, postProwlarrSync(t, a, url, apply))
				if out.Removed != 0 || len(out.Removals) != 0 {
					t.Fatalf("apply=%v removed=%d removals=%v", apply, out.Removed, indexerIDs(out.Removals))
				}
				if !slices.ContainsFunc(out.Warnings, func(w string) bool {
					return strings.Contains(w, "source reported no enabled indexers; nothing removed")
				}) {
					t.Fatalf("apply=%v warnings=%v", apply, out.Warnings)
				}
				if got := storedIDs(store); !slices.Equal(got, want) {
					t.Fatalf("apply=%v ids=%v want %v", apply, got, want)
				}
			}
		})
	}
}

// Same-scope entries whose generated identity (URL or name marker) was
// changed locally are kept and reported as keptEdited, never removed.
func TestProwlarrSyncKeepsLocallyEditedEntries(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":97,"name":"Gone","enable":true,"protocol":"usenet"},{"id":98,"name":"URLEdited","enable":true,"protocol":"usenet"},{"id":99,"name":"NameEdited","enable":true,"protocol":"usenet"}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: url})
	seedProwlarrImported(t, a, url)
	if err := store.Update(func(c *config.Config) error {
		for i := range c.Indexers {
			switch c.Indexers[i].ID {
			case prefix + "98":
				c.Indexers[i].URL = "http://example.com/custom"
			case prefix + "99":
				c.Indexers[i].Name = "My renamed indexer"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	body = `[{"id":4,"name":"News","enable":true,"protocol":"usenet"}]`
	out := decodeSync(t, postProwlarrSync(t, a, url, false))
	if got := indexerIDs(out.Removals); !slices.Equal(got, []string{prefix + "97"}) {
		t.Fatalf("preview removals=%v want only unedited 97", got)
	}
	if got, want := indexerIDs(out.KeptEdited), []string{prefix + "98", prefix + "99"}; !slices.Equal(got, want) {
		t.Fatalf("preview keptEdited=%v want %v", got, want)
	}
	out = decodeSync(t, postProwlarrSync(t, a, url, true))
	if out.Removed != 1 {
		t.Fatalf("apply removed=%d want 1", out.Removed)
	}
	if got, want := storedIDs(store), []string{prefix + "4", prefix + "98", prefix + "99"}; !slices.Equal(got, want) {
		t.Fatalf("after sync ids=%v want %v (edited kept)", got, want)
	}
}

// An incomplete discovery (enabled indexers skipped for an unknown
// protocol) never removes on sync.
func TestProwlarrSyncSkippedUnknownProtocolNeverRemoves(t *testing.T) {
	body := `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":99,"name":"Old","enable":true,"protocol":"usenet"}]`
	status := http.StatusOK
	a, store, url := prowlarrTestApp(t, &body, &status, "")
	prefix := newznab.ProwlarrIDPrefix(config.Prowlarr{URL: url})
	seedProwlarrImported(t, a, url)

	body = `[{"id":4,"name":"News","enable":true,"protocol":"usenet"},{"id":5,"name":"Odd","enable":true,"protocol":"rss"}]`
	for _, apply := range []bool{false, true} {
		out := decodeSync(t, postProwlarrSync(t, a, url, apply))
		if out.SkippedUnknownProtocol != 1 {
			t.Fatalf("apply=%v skipped=%d want 1", apply, out.SkippedUnknownProtocol)
		}
		if out.Removed != 0 || len(out.Removals) != 0 {
			t.Fatalf("apply=%v removed=%d removals=%v", apply, out.Removed, indexerIDs(out.Removals))
		}
		if !slices.ContainsFunc(out.Warnings, func(w string) bool { return strings.Contains(w, "nothing removed") }) {
			t.Fatalf("apply=%v warnings=%v", apply, out.Warnings)
		}
		if got, want := storedIDs(store), []string{prefix + "4", prefix + "99"}; !slices.Equal(got, want) {
			t.Fatalf("apply=%v ids=%v want %v", apply, got, want)
		}
	}
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
