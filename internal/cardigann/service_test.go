package cardigann

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/store"
)

func newTestService(t *testing.T, sites config.IndexerSites) *Service {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(Options{DB: db, Logger: quietLog(), Sites: sites})
}

func TestServiceInstallListRemove(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t, config.IndexerSites{})
	sum, err := s.Install(ctx, readFixture(t, "public.yml"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if sum.ID != "fakepublic" || sum.Source != "upload" || !sum.Movies || len(sum.Settings) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	if _, err := s.Install(ctx, []byte("id: broken\n"), "", ""); err == nil {
		t.Fatal("invalid definition installed")
	}
	if _, err := s.Install(ctx, readFixture(t, "private.yml"), "", ""); err != nil {
		t.Fatal(err)
	}
	list, err := s.Definitions(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "Fake Private" || list[1].Login != "" || list[0].Login != "form" {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if err := s.Remove(ctx, "fakeprivate"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "fakeprivate"); !errors.Is(err, ErrUnknownDefinition) {
		t.Fatalf("second remove = %v", err)
	}
	if _, err := s.Definition(ctx, "fakeprivate"); !errors.Is(err, ErrUnknownDefinition) {
		t.Fatalf("removed definition still served: %v", err)
	}
}

// TestManagerIntegration runs a built-in site through newznab.Manager:
// budgets, the result cache, release stamping and torrent resolution all
// apply as they do for Newznab indexers.
func TestManagerIntegration(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	site := publicSite(t, &seen, &mu)
	defer site.Close()
	ctx := context.Background()
	s := newTestService(t, config.IndexerSites{})
	if _, err := s.Install(ctx, readFixture(t, "public.yml"), "", ""); err != nil {
		t.Fatal(err)
	}
	ix := config.Indexer{ID: "pub", Name: "Public site", URL: site.URL, Kind: config.IndexerKindCardigann, Definition: "fakepublic",
		Protocol: "torrent", Enabled: true, DailyLimit: 2, DailyGrabs: 1, Settings: map[string]string{"sort": "added"}}
	m := newznab.NewManager(nil, []config.Indexer{ix}, newznab.Options{Backend: s.Backend, Logger: quietLog()})
	req := core.SearchRequest{Type: core.TypeMovie, Title: "The Signal", Year: 1999, IDs: core.IDs{IMDB: "tt0133093"}}
	rels, err := m.Search(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 3 || rels[0].Indexer != "Public site" || rels[0].ID == "" || rels[0].Info.Height != 1080 {
		t.Fatalf("rels = %+v", rels)
	}
	// Cached: no new request and no budget spent.
	if _, err := m.Search(ctx, req); err != nil || len(seen) != 1 {
		t.Fatalf("cache miss: %v %v", err, seen)
	}
	rem, _ := m.Remaining(ctx)
	if rem[0].HitsUsed != 1 {
		t.Fatalf("hits used = %d", rem[0].HitsUsed)
	}
	resolved, err := m.ResolveTorrent(ctx, rels[0])
	if err != nil || resolved.InfoHash == "" {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	// The grab budget (1) is now spent.
	other := rels[2]
	if _, err := m.ResolveTorrent(ctx, other); !errors.Is(err, core.ErrBudgetExhausted) {
		t.Fatalf("grab budget not enforced: %v", err)
	}
	if _, err := m.FetchNZB(ctx, rels[0]); err == nil {
		t.Fatal("torrent fetched as NZB")
	}
	// A missing definition surfaces as a search error.
	bad := ix
	bad.ID, bad.Name, bad.Definition = "gone", "Gone", "missing"
	m2 := newznab.NewManager(nil, []config.Indexer{bad}, newznab.Options{Backend: s.Backend, Logger: quietLog()})
	if _, err := m2.Search(ctx, core.SearchRequest{Type: core.TypeMovie, Title: "x"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing definition err = %v", err)
	}
}

func TestServiceTestRecordsStatusAndRebuildsRunners(t *testing.T) {
	site := newPrivateSite(t)
	ctx := context.Background()
	s := newTestService(t, config.IndexerSites{})
	if _, err := s.Install(ctx, readFixture(t, "private.yml"), "", ""); err != nil {
		t.Fatal(err)
	}
	ix := config.Indexer{ID: "priv", Name: "Private", URL: site.srv.URL, Kind: config.IndexerKindCardigann, Definition: "fakeprivate", Enabled: true,
		Settings: map[string]string{"username": "alice"}, SecretSettings: map[string]string{"password": "secret"}}
	res := s.Test(ctx, ix, true)
	if !res.OK || res.Results != 2 || len(res.Titles) != 2 {
		t.Fatalf("test = %+v", res)
	}
	statuses, err := s.Statuses(ctx)
	if err != nil || len(statuses) != 1 || !statuses[0].OK || statuses[0].Message != "2 results" {
		t.Fatalf("statuses = %+v %v", statuses, err)
	}
	// The test signs in on a throwaway session: the saved row is untouched, so
	// the first search logs in itself.
	siteURL, _ := url.Parse(site.srv.URL)
	if _, ok, err := s.sessions.load(ctx, ix.ID, siteOrigin(siteURL)); err != nil || ok {
		t.Fatalf("test wrote the saved session: ok=%v err=%v", ok, err)
	}
	if site.loginCount() != 1 {
		t.Fatalf("test logins = %d", site.loginCount())
	}
	b, err := s.Backend(ix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Search(ctx, core.SearchRequest{Type: core.TypeMovie, Title: "Private Film"}, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if site.loginCount() != 2 {
		t.Fatalf("logins = %d", site.loginCount())
	}
	// Changing the password drops the stored session.
	ix.SecretSettings = map[string]string{"password": "nope"}
	b, err = s.Backend(ix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Search(ctx, core.SearchRequest{Type: core.TypeMovie, Title: "Private Film"}, func(context.Context, string) error { return nil }); !errors.Is(err, ErrLogin) {
		t.Fatalf("stale credentials err = %v", err)
	}
	res = s.Test(ctx, ix, true)
	if res.OK || !strings.Contains(res.Error, "Wrong username or password") {
		t.Fatalf("bad test = %+v", res)
	}
	statuses, _ = s.Statuses(ctx)
	if statuses[0].OK {
		t.Fatal("failed test recorded as ok")
	}
}

func TestListingURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/someone/defs/tree/main/definitions/v11":            "https://api.github.com/repos/someone/defs/contents/definitions/v11?ref=main",
		"https://raw.githubusercontent.com/someone/defs/main/definitions/v11/": "https://api.github.com/repos/someone/defs/contents/definitions/v11?ref=main",
		"https://raw.githubusercontent.com/someone/defs/refs/heads/dev/defs/":  "https://api.github.com/repos/someone/defs/contents/defs?ref=dev",
		"https://api.github.com/repos/someone/defs/contents/definitions?ref=x": "https://api.github.com/repos/someone/defs/contents/definitions?ref=x",
		"https://example.invalid/index.txt":                                    "https://example.invalid/index.txt",
	} {
		got, err := listingURL(in)
		if err != nil || got != want {
			t.Errorf("listingURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := listingURL("file:///etc"); err == nil {
		t.Error("non-http source accepted")
	}
}

func TestParseListingFormats(t *testing.T) {
	base, _ := url.Parse("https://src.invalid/defs/index.json")
	gh := parseListing([]byte(`[{"name":"a.yml","type":"file","sha":"1","download_url":"https://raw.invalid/a.yml"},{"name":"README.md","type":"file"},{"name":"sub","type":"dir"}]`), base)
	if len(gh) != 1 || gh[0].ID != "a" || gh[0].SHA != "1" || gh[0].URL != "https://raw.invalid/a.yml" {
		t.Fatalf("github = %+v", gh)
	}
	names := parseListing([]byte(`["b.yml", "https://other.invalid/c.yaml"]`), base)
	if len(names) != 2 || names[0].URL != "https://src.invalid/defs/b.yml" || names[1].ID != "c" {
		t.Fatalf("names = %+v", names)
	}
	text := parseListing([]byte("# comment\nd.yml\n\nsub/e.yml\nnotes.txt\n"), base)
	if len(text) != 2 || text[1].URL != "https://src.invalid/defs/sub/e.yml" || text[1].ID != "e" {
		t.Fatalf("text = %+v", text)
	}
}

// sourceServer serves a GitHub-contents-style listing of the fixtures.
type sourceServer struct {
	mu    sync.Mutex
	files map[string][]byte
	shas  map[string]string
	srv   *httptest.Server
}

func newSourceServer(t *testing.T, files ...string) *sourceServer {
	t.Helper()
	s := &sourceServer{files: map[string][]byte{}, shas: map[string]string{}}
	for _, f := range files {
		s.files[f] = readFixture(t, f)
		s.shas[f] = "sha-1"
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Path == "/listing" {
			var out []map[string]string
			for name := range s.files {
				out = append(out, map[string]string{"name": name, "type": "file", "sha": s.shas[name], "download_url": s.srv.URL + "/raw/" + name})
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		if b, ok := s.files[strings.TrimPrefix(r.URL.Path, "/raw/")]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func TestCatalogInstallAndUpdate(t *testing.T) {
	ctx := context.Background()
	src := newSourceServer(t, "public.yml", "json.yml")
	s := newTestService(t, config.IndexerSites{})
	if _, err := s.Catalog(ctx, false); !errors.Is(err, ErrNoSource) {
		t.Fatalf("no source err = %v", err)
	}
	s.ApplyConfig(config.Config{IndexerSites: config.IndexerSites{DefinitionsURL: src.srv.URL + "/listing"}})
	entries, err := s.Catalog(ctx, false)
	if err != nil || len(entries) != 2 || entries[0].ID != "json" || entries[0].Installed {
		t.Fatalf("catalog = %+v err=%v", entries, err)
	}
	// The file is named json.yml but declares id fakejson.
	sum, err := s.InstallFromSource(ctx, "json")
	if err != nil || sum.ID != "fakejson" || sum.Source != "source" {
		t.Fatalf("install = %+v err=%v", sum, err)
	}
	if _, err := s.InstallFromSource(ctx, "public"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallFromSource(ctx, "nope"); err == nil {
		t.Fatal("unknown catalog id installed")
	}
	// Uploaded definitions are not touched by updates.
	if _, err := s.Install(ctx, readFixture(t, "rss.yml"), "", ""); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Update(ctx)
	if err != nil || len(rep.Updated) != 0 || rep.Unchanged != 2 || len(rep.Failed) != 0 {
		t.Fatalf("no-op update = %+v err=%v", rep, err)
	}
	// The source publishes a new revision of the public definition.
	src.mu.Lock()
	src.files["public.yml"] = bytes.Replace(src.files["public.yml"], []byte("name: Fake Public"), []byte("name: Fake Public v2"), 1)
	src.shas["public.yml"] = "sha-2"
	src.files["json.yml"] = []byte("id: fakejson\n")
	src.shas["json.yml"] = "sha-2"
	src.mu.Unlock()
	rep, err = s.Update(ctx)
	if err != nil || len(rep.Updated) != 1 || rep.Updated[0] != "fakepublic" || len(rep.Failed) != 1 || rep.Failed[0].ID != "fakejson" {
		t.Fatalf("update = %+v err=%v", rep, err)
	}
	sum, _ = s.Definition(ctx, "fakepublic")
	if sum.Name != "Fake Public v2" {
		t.Fatalf("updated name = %q", sum.Name)
	}
}

func TestHandler(t *testing.T) {
	ctx := context.Background()
	site := newPrivateSite(t)
	s := newTestService(t, config.IndexerSites{})
	indexers := []config.Indexer{{ID: "priv", Name: "Private", Kind: config.IndexerKindCardigann, Definition: "fakeprivate"}}
	h := s.Handler(HandlerDeps{
		Indexers: func() []config.Indexer { return indexers },
		Unredact: func(ix config.Indexer) config.Indexer {
			if ix.SecretSettings["password"] == "••••" {
				ix.SecretSettings["password"] = "secret"
			}
			return ix
		},
	})
	do := func(method, path string, body any) (int, map[string]any) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, out := do("POST", "/definitions", map[string]string{"yaml": "id: broken\n"}); code != 400 || out["error"].(map[string]any)["code"] != "invalid_definition" {
		t.Fatalf("invalid upload = %d %v", code, out)
	}
	if code, out := do("POST", "/definitions", map[string]string{"yaml": string(readFixture(t, "private.yml"))}); code != 200 || out["definition"].(map[string]any)["id"] != "fakeprivate" {
		t.Fatalf("upload = %d %v", code, out)
	}
	if code, out := do("GET", "/definitions", nil); code != 200 || len(out["definitions"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, _ := do("GET", "/definitions/fakeprivate", nil); code != 200 {
		t.Fatalf("get = %d", code)
	}
	if code, _ := do("GET", "/definitions/nope", nil); code != 404 {
		t.Fatalf("get missing = %d", code)
	}
	if code, _ := do("GET", "/catalog", nil); code != 409 {
		t.Fatalf("catalog without source = %d", code)
	}
	code, out := do("POST", "/test", config.Indexer{ID: "priv", Name: "Private", URL: site.srv.URL, Kind: config.IndexerKindCardigann, Definition: "fakeprivate",
		Settings: map[string]string{"username": "alice"}, SecretSettings: map[string]string{"password": "••••"}})
	if code != 200 || out["ok"] != true || out["results"].(float64) != 2 {
		t.Fatalf("test = %d %v", code, out)
	}
	if code, out := do("GET", "/status", nil); code != 200 || len(out["statuses"].([]any)) != 1 {
		t.Fatalf("status = %d %v", code, out)
	}
	if code, out := do("DELETE", "/definitions/fakeprivate", nil); code != 409 || !strings.Contains(fmt.Sprint(out), "Private") {
		t.Fatalf("delete in use = %d %v", code, out)
	}
	indexers = nil
	if code, _ := do("DELETE", "/definitions/fakeprivate", nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code, out := do("POST", "/update", nil); code != 200 || out["unchanged"].(float64) != 0 {
		t.Fatalf("update = %d %v", code, out)
	}
}

func TestDefinitionCacheSeesOtherNodesUpdates(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := testNow
	clock := func() time.Time { return now }
	a := New(Options{DB: db, Logger: quietLog(), Now: clock})
	b := New(Options{DB: db, Logger: quietLog(), Now: clock})
	if _, err := a.Install(ctx, readFixture(t, "public.yml"), "", ""); err != nil {
		t.Fatal(err)
	}
	if sum, err := b.Definition(ctx, "fakepublic"); err != nil || sum.Name != "Fake Public" {
		t.Fatalf("node b = %+v %v", sum, err)
	}
	if _, err := a.Install(ctx, bytes.Replace(readFixture(t, "public.yml"), []byte("name: Fake Public"), []byte("name: Renamed"), 1), "", ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * definitionRecheck)
	if sum, _ := b.Definition(ctx, "fakepublic"); sum.Name != "Renamed" {
		t.Fatalf("node b kept a stale definition: %q", sum.Name)
	}
}

// The parsed-definition cache read checkedAt outside the lock that its
// refresh writes it under: concurrent searches past the recheck interval
// raced (go test -race).
func TestDefinitionCacheConcurrentRecheck(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	now := testNow
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(definitionRecheck)
		return now
	}
	s := New(Options{Logger: quietLog(), Now: clock})
	if _, err := s.Install(ctx, readFixture(t, "public.yml"), "", ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := s.definition(ctx, "fakepublic"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
