package route

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRouter(t *testing.T) {
	var r Router
	err := r.Handle("get", "/Items/{id}", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte(req.PathValue("id"))) }))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Handle("GET", "/items/{ID}", http.NotFoundHandler()); err == nil {
		t.Fatal("duplicate")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/iTeMs/AbC", nil))
	if w.Code != 200 || w.Body.String() != "AbC" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/items/AbC", nil))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	routes := r.Routes()
	routes[0].Pattern = "changed"
	if r.Routes()[0].Pattern != "/Items/{id}" {
		t.Fatal("inventory aliased")
	}
}
func TestWildcardAndQuery(t *testing.T) {
	var r Router
	if err := r.Handle("GET", "/Media/{path...}", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte(req.PathValue("path"))) })); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/media/A/B", nil))
	if w.Body.String() != "A/B" {
		t.Fatal(w.Body.String())
	}
	if got := Query(url.Values{"aPi_KeY": {"a"}}, "api_key"); len(got) != 1 || got[0] != "a" {
		t.Fatal(got)
	}
}

func TestRouterPrefersLiteralsThenParametersThenTail(t *testing.T) {
	var r Router
	for _, item := range []struct{ pattern, value string }{
		{"/items/{tail...}", "tail"}, {"/items/{id}", "parameter"}, {"/Items/Latest", "literal"},
		{"/items/{id}/details", "detail"}, {"/items/fixed/{id}", "fixed"},
	} {
		value := item.value
		if err := r.Handle("GET", item.pattern, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(value)) })); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ path, want string }{
		{"/ITEMS/latest", "literal"}, {"/items/other", "parameter"}, {"/items/other/details", "detail"}, {"/items/fixed/details", "fixed"}, {"/items/a/b/c", "tail"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != 200 || w.Body.String() != tc.want {
			t.Fatalf("%s: %d %q", tc.path, w.Code, w.Body.String())
		}
	}
	if got := r.Routes()[0].Pattern; got != "/items/{tail...}" {
		t.Fatalf("inventory order changed: %s", got)
	}
}

func TestUnmatchedObserverRunsOutsideLock(t *testing.T) {
	var r Router
	if err := r.Handle("GET", "/item", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err != nil {
		t.Fatal(err)
	}
	var statuses []int
	r.SetUnmatched(func(_ *http.Request, status int) {
		statuses = append(statuses, status)
		_ = r.Routes()
		r.SetUnmatched(nil)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/item", nil))
	if w.Code != 405 || len(statuses) != 1 || statuses[0] != 405 {
		t.Fatalf("method: %d %v", w.Code, statuses)
	}
	r.SetUnmatched(func(_ *http.Request, status int) { statuses = append(statuses, status) })
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/missing", nil))
	if w.Code != 404 || len(statuses) != 2 || statuses[1] != 404 {
		t.Fatalf("missing: %d %v", w.Code, statuses)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/item", nil))
	if len(statuses) != 2 {
		t.Fatalf("observer called on match: %v", statuses)
	}
}
