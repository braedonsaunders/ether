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
