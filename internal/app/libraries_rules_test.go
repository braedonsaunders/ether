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

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestLibrariesConsoleRulesPreviewAndPluginsSetup(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	rel := time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, title := range []core.Title{
		{Key: core.MovieKey(201), Type: core.TypeMovie, Title: "US Action", Language: "en", Genres: []string{"Action"}, Released: rel, HomeReleased: rel},
		{Key: core.MovieKey(202), Type: core.TypeMovie, Title: "FR Action", Language: "fr", Genres: []string{"Action"}, Released: rel, HomeReleased: rel},
		{Key: core.MovieKey(203), Type: core.TypeMovie, Title: "JP Action", Language: "ja", Genres: []string{"Action"}, Released: rel, HomeReleased: rel},
		{Key: core.ShowKey(301), Type: core.TypeShow, Title: "FR Drama Show", Language: "fr", Genres: []string{"Drama"}, Released: rel},
	} {
		if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}

	c := &librariesConsole{app: a}
	h := c.handler()
	call := func(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	// Save a canonical Movies rule (hide French movies) via PATCH /1; TV Shows remain unaffected.
	w := call(h, "PATCH", "/1", `{"rules":[{"name":"No French Movies","mediaType":"movie","match":"all","rules":[{"field":"language","op":"is","value":"fr"}]}],"visibilityDefault":"show"}`)
	if w.Code != 200 {
		t.Fatalf("patch movies rules %d %s", w.Code, w.Body)
	}
	if a.catalog.Visible(&core.Title{Key: core.MovieKey(202), Type: core.TypeMovie, Language: "fr", Released: rel}) {
		t.Fatal("canonical Movies rule did not apply to catalog visibility")
	}
	if !a.catalog.Visible(&core.Title{Key: core.ShowKey(301), Type: core.TypeShow, Language: "fr", Released: rel}) {
		t.Fatal("Movies rule leaked onto TV Shows")
	}

	// Create an Action saved-filter library and add an extra rule hiding Japanese movies on it.
	w = call(h, "POST", "/", `{"name":"Action Picks","mediaType":"movie"}`)
	if w.Code != 201 {
		t.Fatalf("create filter library %d %s", w.Code, w.Body)
	}
	var filterLib libraryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &filterLib); err != nil {
		t.Fatal(err)
	}
	fid := strconv.FormatInt(int64(filterLib.ID), 10)
	w = call(h, "PATCH", "/"+fid, `{"rules":[{"name":"No JP","mediaType":"movie","match":"all","rules":[{"field":"language","op":"is","value":"ja"}]}]}`)
	if w.Code != 200 {
		t.Fatalf("patch filter library rules %d %s", w.Code, w.Body)
	}

	// Browsing the filter library hides both FR (canonical Movies rule) and JP (filter library rule), leaving US Action.
	w = call(h, "GET", "/"+fid+"/items", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "US Action") || strings.Contains(w.Body.String(), "FR Action") || strings.Contains(w.Body.String(), "JP Action") {
		t.Fatalf("filter library items = %d %s", w.Code, w.Body)
	}

	// Scoped preview for the filter library sees 2 canonical candidates and 1 hidden by extra rule.
	freshLib, err := a.libraries.Get(ctx, filterLib.ID)
	if err != nil {
		t.Fatal(err)
	}
	sf := libraryScopeFilter(freshLib, core.Filter{RuleVisibility: "all"})
	rawPrev, err := a.catalog.PreviewVisibilityScoped(ctx, freshLib.Rules, freshLib.VisibilityDefault, &sf)
	if err != nil {
		t.Fatal(err)
	}
	prev := rawPrev.(catalog.VisibilityPreview)
	if prev.Total != 2 || prev.Hidden != 1 || prev.Visible != 1 {
		t.Fatalf("admin CatalogVisibility for filter library = %+v", prev)
	}

	// GET / on plugins-setup lists content-kind plugins (empty when none installed).
	sh := c.setupHandler()
	w = call(sh, "GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"plugins"`) {
		t.Fatalf("plugins-setup list = %d %s", w.Code, w.Body)
	}
}
