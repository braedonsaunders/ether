package extend

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func step(id string, order int) Descriptor {
	return Descriptor{Kind: KindRankingStep, ID: id, Owner: Builtin, Label: T("Step " + id), Order: order}
}

func TestRegisterValidatesAndOrders(t *testing.T) {
	r := New()
	if err := r.Register(step("b", 2), step("a", 1)); err != nil {
		t.Fatal(err)
	}
	got := r.List(KindRankingStep)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("order = %+v", got)
	}
	if err := r.Register(step("a", 3)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate error = %v", err)
	}
	bad := []Descriptor{
		{Kind: "nope", ID: "x", Owner: Builtin, Label: T("x")},
		{Kind: KindPage, ID: "", Owner: Builtin, Label: T("x")},
		{Kind: KindPage, ID: "x", Owner: Builtin},
		{Kind: KindPage, ID: "x", Owner: "dev.example", Label: PluginText("dev.example", "k")},
		{Kind: KindPage, ID: "plugin:x/y", Owner: Builtin, Label: T("x")},
		{Kind: KindPage, ID: "x", Owner: Builtin, Label: PluginText("p", "k")},
	}
	for _, d := range bad {
		if err := r.Register(d); err == nil {
			t.Fatalf("expected error for %+v", d)
		}
	}
	if len(r.List()) != 2 {
		t.Fatal("failed registrations must not add anything")
	}
}

func TestReplaceOwnerIsAtomic(t *testing.T) {
	r := New()
	r.MustRegister(step("language", 1))
	calls := 0
	unsub := r.Subscribe(func() { calls++ })
	defer unsub()
	p := "dev.example"
	d := Descriptor{Kind: KindRankingStep, ID: PluginID(p, "dub"), Owner: p, Label: PluginText(p, "step.dub")}
	if err := r.ReplaceOwner(p, []Descriptor{d}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(KindRankingStep, d.ID); !ok {
		t.Fatal("plugin descriptor missing")
	}
	clash := Descriptor{Kind: KindRankingStep, ID: "language", Owner: p, Label: PluginText(p, "x")}
	if err := r.ReplaceOwner(p, []Descriptor{clash}); err == nil {
		t.Fatal("expected namespace error")
	}
	if _, ok := r.Get(KindRankingStep, d.ID); !ok {
		t.Fatal("a failed replace must keep the previous descriptors")
	}
	r.RemoveOwner(p)
	if len(r.Owned(p)) != 0 || len(r.List()) != 1 {
		t.Fatalf("remove owner left %+v", r.List())
	}
	if err := r.ReplaceOwner(Builtin, nil); err == nil {
		t.Fatal("built-ins cannot be replaced")
	}
	if calls != 2 {
		t.Fatalf("subscriber calls = %d, want 2", calls)
	}
}

func TestMeta(t *testing.T) {
	type meta struct{ Editor string }
	d := step("x", 0).WithMeta(meta{Editor: "choice"})
	got, err := MetaOf[meta](d)
	if err != nil || got.Editor != "choice" {
		t.Fatalf("meta = %+v, %v", got, err)
	}
}

func TestHandler(t *testing.T) {
	r := New()
	p := "dev.example"
	r.MustRegister(step("language", 1))
	if err := r.ReplaceOwner(p, []Descriptor{{Kind: KindPage, ID: PluginID(p, "home"), Owner: p, Label: PluginText(p, "page.title")}}); err != nil {
		t.Fatal(err)
	}
	h := Handler(r, func(plugin, key, locale string) (string, bool) {
		if plugin == p && key == "page.title" && locale == "fr" {
			return "Accueil", true
		}
		return "", false
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?kinds=page&locale=fr", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Descriptors) != 1 || body.Descriptors[0].Label.Resolved != "Accueil" {
		t.Fatalf("body = %+v", body)
	}
	etag := rec.Header().Get("ETag")
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?kinds=page&locale=fr", nil)
	req.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("revalidate status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?kinds=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind status %d", rec.Code)
	}
}
