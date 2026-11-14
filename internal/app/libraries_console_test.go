package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/plugins"
)

func TestLibrariesConsoleCRUDAndFilterBrowse(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	for _, title := range []core.Title{
		{Key: core.MovieKey(101), Type: core.TypeMovie, Title: "Alpha Drama", Year: 2001, Released: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2001, 6, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Drama"}},
		{Key: core.MovieKey(102), Type: core.TypeMovie, Title: "Beta Laughs", Year: 2002, Released: time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2002, 6, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Comedy"}},
	} {
		if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	c := &librariesConsole{app: a}
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// Built-ins are seeded and listed.
	w := call("GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"key":"movies"`) {
		t.Fatalf("list %d %s", w.Code, w.Body)
	}

	// Create a console-only filter library (spec §15.3).
	w = call("POST", "/", `{"name":"Dramas","mediaType":"movie","rules":[{"action":"show","mediaType":"movie","match":"all","rules":[{"field":"genre","op":"in","value":["Drama"]}]}],"visibilityDefault":"hide"}`)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var lib libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &lib); err != nil {
		t.Fatal(err)
	}
	if lib.Source != core.LibraryCatalog || lib.Present.Plex != nil || lib.Present.MediaBrowser != nil {
		t.Fatalf("new filter library must be console-only: %+v", lib.Present)
	}
	id := strconv.FormatInt(int64(lib.ID), 10)

	// Browsing it shows only the filtered set.
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Alpha Drama") || strings.Contains(w.Body.String(), "Beta Laughs") {
		t.Fatalf("filter browse %d %s", w.Code, w.Body)
	}

	// Rename + hide + show-in-apps (Plex) through PATCH.
	w = call("PATCH", "/"+id, `{"name":"Serious films","hidden":true,"present":{"plex":{"base":"movie"}}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Serious films") || !strings.Contains(w.Body.String(), `"hidden":true`) {
		t.Fatalf("patch %d %s", w.Code, w.Body)
	}

	// The saved filter can be replaced under the same id.
	if w := call("PATCH", "/"+id, `{"rules":[],"visibilityDefault":"show"}`); w.Code != 200 {
		t.Fatalf("filter patch %d %s", w.Code, w.Body)
	}

	// Built-ins cannot be deleted; the filter library can.
	if w := call("DELETE", "/1", ""); w.Code != 409 {
		t.Fatalf("builtin delete %d", w.Code)
	}
	if w := call("DELETE", "/"+id, ""); w.Code != 204 {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/"+id, ""); w.Code != 404 {
		t.Fatalf("get deleted %d", w.Code)
	}
}

func TestLibrariesConsoleCollectionSource(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	for _, title := range []core.Title{
		{Key: core.MovieKey(201), Type: core.TypeMovie, Title: "Listed Film", Year: 2005, Released: time.Date(2005, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2005, 6, 1, 0, 0, 0, 0, time.UTC)},
		{Key: core.MovieKey(202), Type: core.TypeMovie, Title: "Unlisted Film", Year: 2006, Released: time.Date(2006, 1, 1, 0, 0, 0, 0, time.UTC), HomeReleased: time.Date(2006, 6, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	// Persist a collection whose members are exactly the listed film.
	now := time.Now().UTC()
	for _, stmt := range []string{
		`INSERT INTO collections_specs(id,slug,builtin,owner_user_id,spec_json,enabled,created_at,updated_at,last_built_at,build_hash,changed_at)
		 VALUES(31,'',0,0,'{}',1,?,?,?,'built',?)`,
		`INSERT INTO collections_members(spec_id,pos,key,shared) VALUES(31,0,?,1)`,
	} {
		args := []any{now.Unix(), now.Unix(), now.Unix(), now.Unix()}
		if strings.Contains(stmt, "collections_members") {
			args = []any{int64(core.MovieKey(201))}
		}
		if _, err := a.db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatal(err)
		}
	}

	c := &librariesConsole{app: a}
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := call("GET", "/collection-sources", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":31`) {
		t.Fatalf("collection sources %d %s", w.Code, w.Body)
	}

	w = call("POST", "/", `{"name":"MDBList picks","collectionId":31,"mediaType":"movie"}`)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var lib libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &lib); err != nil {
		t.Fatal(err)
	}
	if lib.Source != core.LibraryCollection || lib.CollectionID != 31 || lib.MediaType != core.TypeMovie {
		t.Fatalf("created library = %+v", lib)
	}
	if lib.Present.Plex != nil || lib.Present.MediaBrowser != nil {
		t.Fatalf("new collection library must be console-only: %+v", lib.Present)
	}
	id := strconv.FormatInt(int64(lib.ID), 10)
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Listed Film") || strings.Contains(w.Body.String(), "Unlisted Film") {
		t.Fatalf("collection browse %d %s", w.Code, w.Body)
	}

	// Deleting the collection leaves the library empty, never a 500.
	if _, err := a.db.ExecContext(ctx, `DELETE FROM collections_members WHERE spec_id=31`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM collections_specs WHERE id=31`); err != nil {
		t.Fatal(err)
	}
	w = call("GET", "/"+id+"/items", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "Listed Film") {
		t.Fatalf("deleted collection browse %d %s", w.Code, w.Body)
	}
	if w = call("GET", "/"+id, ""); w.Code != 200 {
		t.Fatalf("deleted collection decorate %d %s", w.Code, w.Body)
	}
}

func TestLibrariesConsolePluginItemMatchPinClear(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	lib, err := a.libraries.UpsertPluginLibrary(ctx, "demo", "book", libraries.PluginLibraryDecl{Name: "Books"})
	if err != nil {
		t.Fatal(err)
	}
	c := &librariesConsole{app: a}
	items, _, err := c.services()
	if err != nil {
		t.Fatal(err)
	}
	decl := plugins.MediaKindMeta{
		Key: "book", Label: "Books",
		Library: plugins.KindLibraryDecl{Name: "Books"},
		Levels:  []plugins.KindLevel{{Key: "book", Label: "Book", Playable: true}},
		Fields: []plugins.KindField{
			{Key: "author", Type: "string", Label: "Author", Indexed: true},
		},
		Browse: plugins.KindBrowseDecl{Facets: []string{"author"}, Search: true},
		Sync:   plugins.KindSyncDecl{Mode: plugins.KindSyncPush},
	}
	if _, err := items.UpsertKind(ctx, "demo", lib.ID, decl, false); err != nil {
		t.Fatal(err)
	}
	if _, err := items.UpsertItems(ctx, "demo", "book", []core.KindItem{{
		ID: "b1", Level: "book", Title: "Dune", Playable: true,
		Fields: map[string]json.RawMessage{"author": json.RawMessage(`"Herbert"`)},
	}}); err != nil {
		t.Fatal(err)
	}
	ref := string(core.NewItemRef("demo", "book", "b1"))
	base := "/" + strconv.FormatInt(int64(lib.ID), 10)
	h := c.handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", base+"/items", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Dune") {
		t.Fatalf("items %d %s", w.Code, w.Body)
	}
	if w := call("GET", base+"/facets/author", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "Herbert") {
		t.Fatalf("facets %d %s", w.Code, w.Body)
	}
	if w := call("PUT", base+"/item/"+ref+"/match", `{"candidateRef":"file-dune-epub"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"chosen":true`) {
		t.Fatalf("pin %d %s", w.Code, w.Body)
	}
	if w := call("DELETE", base+"/item/"+ref+"/match", ""); w.Code != 200 || strings.Contains(w.Body.String(), "file-dune-epub") {
		t.Fatalf("clear %d %s", w.Code, w.Body)
	}
	// Plugin libraries are removed with their plugin, not from here.
	if w := call("DELETE", base, ""); w.Code != 409 {
		t.Fatalf("plugin delete %d", w.Code)
	}
	// Cache view is available for plugin libraries only.
	if w := call("GET", base+"/cache", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"maxBytes":2147483648`) {
		t.Fatalf("cache %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/1/cache", ""); w.Code != 404 {
		t.Fatalf("builtin cache %d", w.Code)
	}
	_ = pluginitems.DefaultMatchThreshold
}
