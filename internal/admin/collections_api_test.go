package admin_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type fieldErr map[string]string

func (f fieldErr) Error() string                    { return "invalid" }
func (f fieldErr) InvalidFields() map[string]string { return f }

type flagErr struct {
	msg                            string
	notConfigured, conflict, upstr bool
}

func (e flagErr) Error() string       { return e.msg }
func (e flagErr) NotConfigured() bool { return e.notConfigured }
func (e flagErr) Conflict() bool      { return e.conflict }
func (e flagErr) Upstream() bool      { return e.upstr }

// fakeStudio is an in-memory CollectionStudio.
type fakeStudio struct {
	specs    map[string]core.CollectionSpec
	genErr   error
	lastPrev core.CollectionSpec
}

func newFakeStudio() *fakeStudio {
	built := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	return &fakeStudio{specs: map[string]core.CollectionSpec{
		"1":               {ID: "1", Name: "Mine", MediaType: "movie", ItemCount: 3, LastBuiltAt: &built},
		"top-rated-movie": {ID: "top-rated-movie", Name: "Top Rated Movies", BuiltIn: true},
	}}
}

func (f *fakeStudio) ListSpecs(ctx context.Context) ([]core.CollectionSpec, error) {
	return []core.CollectionSpec{f.specs["top-rated-movie"], f.specs["1"]}, nil
}

func (f *fakeStudio) GetSpec(ctx context.Context, id string) (*core.CollectionSpec, error) {
	s, ok := f.specs[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &s, nil
}

func (f *fakeStudio) CreateSpec(ctx context.Context, s core.CollectionSpec) (*core.CollectionSpec, error) {
	if s.Name == "" {
		return nil, fieldErr{"name": "a name is required"}
	}
	s.ID = "2"
	f.specs["2"] = s
	return &s, nil
}

func (f *fakeStudio) UpdateSpec(ctx context.Context, id string, s core.CollectionSpec) (*core.CollectionSpec, error) {
	if _, ok := f.specs[id]; !ok {
		return nil, core.ErrNotFound
	}
	s.ID = id
	f.specs[id] = s
	return &s, nil
}

func (f *fakeStudio) DeleteSpec(ctx context.Context, id string) error {
	if f.specs[id].BuiltIn {
		return flagErr{msg: "built-ins cannot be deleted", conflict: true}
	}
	if _, ok := f.specs[id]; !ok {
		return core.ErrNotFound
	}
	delete(f.specs, id)
	return nil
}

func (f *fakeStudio) Preview(ctx context.Context, s core.CollectionSpec, n int) (*core.CollectionPreview, error) {
	f.lastPrev = s
	return &core.CollectionPreview{Total: 42, TookMs: 7, Warnings: []string{"heads up"}, Items: []core.Title{
		{Key: core.MovieKey(5), Type: core.TypeMovie, Title: "Five", Year: 1999, PosterPath: "/five.jpg", BackdropPath: "/bd.jpg",
			Runtime: 95 * time.Minute, Rating: 7.86, Genres: []string{"Drama"}},
	}}, nil
}

func (f *fakeStudio) Rebuild(ctx context.Context, id string) (*core.CollectionSpec, error) {
	return f.GetSpec(ctx, id)
}

func (f *fakeStudio) Templates() []core.CollectionTemplate {
	return []core.CollectionTemplate{{ID: "t1", Name: "T", Category: "Moods", Spec: core.CollectionSpec{Name: "T"}}}
}

func (f *fakeStudio) Generate(ctx context.Context, prompt, media string) (*core.CollectionSpec, string, error) {
	if f.genErr != nil {
		return nil, "", f.genErr
	}
	return &core.CollectionSpec{Name: "Generated " + prompt, MediaType: media}, "because", nil
}

func (f *fakeStudio) Artwork(ctx context.Context, id string, w int) ([]byte, string, time.Time, error) {
	if _, ok := f.specs[id]; !ok {
		return nil, "", time.Time{}, core.ErrNotFound
	}
	return []byte("JPEGDATA"), "image/jpeg", time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC), nil
}

type fakeHelper struct{}

func (fakeHelper) SearchTitles(ctx context.Context, q string, t core.MediaType, limit int) ([]core.Title, error) {
	return []core.Title{{Key: core.ShowKey(9), Type: core.TypeShow, Title: q + " show", PosterPath: "/s.jpg"}}, nil
}

func (fakeHelper) Facets(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	if facet == "mood" {
		return nil, fieldErr{"facet": "unknown facet"}
	}
	return []core.FacetValue{{Key: "ko", Title: "Korean", Count: 3}}, nil
}

func (fakeHelper) Lookup(ctx context.Context, kind, q, region string) ([]core.LookupResult, error) {
	return []core.LookupResult{{ID: 525, Name: "Christopher Nolan", Subtitle: "Directing " + region, ImagePath: "/nolan.jpg"}}, nil
}

func (fakeHelper) TitleDetail(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	if key != core.MovieKey(7) {
		return nil, core.ErrNotFound
	}
	return &core.Title{Key: key, Type: core.TypeMovie, Title: "Seven", Keywords: []string{"heist"}, CollectionID: 10, CollectionName: "Saga",
		Cast: []core.Person{{TMDB: 1, Name: "Actor", Role: "Lead", ThumbPath: "/a.jpg"}}, Directors: []core.Person{{TMDB: 2, Name: "Director"}}}, nil
}

type fakeImages struct{ calls int }

func (f *fakeImages) Get(ctx context.Context, path string, w int) ([]byte, string, error) {
	f.calls++
	if path == "/missing.jpg" {
		return nil, "", errors.New("artwork: image not found")
	}
	return []byte("IMG" + path), "image/jpeg", nil
}

func studioHandler(t *testing.T) (http.Handler, *fakeStudio, *fakeImages) {
	t.Helper()
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	st := newFakeStudio()
	im := &fakeImages{}
	return admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Collections: st, CatalogHelper: fakeHelper{}, Images: im}), st, im
}

func authed(r *http.Request) *http.Request {
	r.Header.Set("X-Plex-Token", ownerToken)
	return r
}

func TestCollectionsAPI(t *testing.T) {
	h, st, _ := studioHandler(t)

	rec := serve(h, authed(req(http.MethodGet, "/ether/api/collections", "")))
	var list struct {
		Collections []struct {
			ID         string `json:"id"`
			BuiltIn    bool   `json:"builtIn"`
			ArtworkURL string `json:"artworkUrl"`
			ItemCount  int    `json:"itemCount"`
		} `json:"collections"`
	}
	decode(t, rec, &list)
	if rec.Code != 200 || len(list.Collections) != 2 || list.Collections[0].ID != "1" || !list.Collections[1].BuiltIn {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(list.Collections[0].ArtworkURL, "/ether/api/collections/1/artwork?v=") {
		t.Fatalf("artwork url: %q", list.Collections[0].ArtworkURL)
	}

	// Create: validation errors carry fields; unknown fields from the UI are tolerated.
	rec = serve(h, authed(req(http.MethodPost, "/ether/api/collections", `{"name":"","artworkUrl":"x"}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"field":"name"`) {
		t.Fatalf("create invalid: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, authed(req(http.MethodPost, "/ether/api/collections", `{"name":"New","mediaType":"show","targets":{"users":"all","collectionsTab":true}}`)))
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"id":"2"`) || !st.specs["2"].Targets.Users.All {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(h, authed(req(http.MethodPost, "/ether/api/collections", `{"name":`))); rec.Code != 400 {
		t.Fatalf("bad json: %d", rec.Code)
	}

	// Get / update / delete and error mapping.
	if rec := serve(h, authed(req(http.MethodGet, "/ether/api/collections/nope", ""))); rec.Code != 404 {
		t.Fatalf("get missing: %d", rec.Code)
	}
	rec = serve(h, authed(req(http.MethodPut, "/ether/api/collections/1", `{"name":"Renamed","mediaType":"movie"}`)))
	if rec.Code != 200 || st.specs["1"].Name != "Renamed" {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(h, authed(req(http.MethodDelete, "/ether/api/collections/top-rated-movie", ""))); rec.Code != 409 {
		t.Fatalf("delete built-in: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(h, authed(req(http.MethodDelete, "/ether/api/collections/2", ""))); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}

	// Preview returns TitleCards with proxied images.
	rec = serve(h, authed(req(http.MethodPost, "/ether/api/collections/preview", `{"name":"P","sources":[{"kind":"catalog"}],"rules":[{"field":"genre","op":"is","value":"Drama"}]}`)))
	var prev struct {
		Total int `json:"total"`
		Items []struct {
			Key            string  `json:"key"`
			Poster         string  `json:"poster"`
			Backdrop       string  `json:"backdrop"`
			RuntimeMinutes int     `json:"runtimeMinutes"`
			Rating         float64 `json:"rating"`
		} `json:"items"`
		Warnings []string `json:"warnings"`
		TookMs   int64    `json:"tookMs"`
	}
	decode(t, rec, &prev)
	if rec.Code != 200 || prev.Total != 42 || len(prev.Items) != 1 || prev.TookMs != 7 || prev.Warnings[0] != "heads up" {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	it := prev.Items[0]
	if it.Key != core.MovieKey(5).String() || it.Poster != "/ether/api/image?path=%2Ffive.jpg&w=342" || it.RuntimeMinutes != 95 || it.Rating != 7.9 {
		t.Fatalf("title card: %+v", it)
	}
	if string(st.lastPrev.Rules[0].Value) != `"Drama"` {
		t.Fatalf("rule value not passed through: %s", st.lastPrev.Rules[0].Value)
	}

	rec = serve(h, authed(req(http.MethodPost, "/ether/api/collections/1/rebuild", "")))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"itemCount"`) || !strings.Contains(rec.Body.String(), `"lastBuiltAt"`) {
		t.Fatalf("rebuild: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, authed(req(http.MethodGet, "/ether/api/collections/templates", "")))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"templates":[{"id":"t1"`) {
		t.Fatalf("templates: %d %s", rec.Code, rec.Body)
	}

	// Generate: prompt required; LLM off is 501; model failure 502.
	if rec := serve(h, authed(req(http.MethodPost, "/ether/api/collections/generate", `{"prompt":" "}`))); rec.Code != 400 {
		t.Fatalf("empty prompt: %d", rec.Code)
	}
	rec = serve(h, authed(req(http.MethodPost, "/ether/api/collections/generate", `{"prompt":"cosy","mediaType":"movie"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"explanation":"because"`) || !strings.Contains(rec.Body.String(), "Generated cosy") {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body)
	}
	st.genErr = flagErr{msg: "LLM off", notConfigured: true}
	if rec := serve(h, authed(req(http.MethodPost, "/ether/api/collections/generate", `{"prompt":"x"}`))); rec.Code != 501 {
		t.Fatalf("llm off: %d", rec.Code)
	}
	st.genErr = flagErr{msg: "garbage", upstr: true}
	if rec := serve(h, authed(req(http.MethodPost, "/ether/api/collections/generate", `{"prompt":"x"}`))); rec.Code != 502 {
		t.Fatalf("llm garbage: %d", rec.Code)
	}

	// Mutations need the CSRF header.
	r := authed(req(http.MethodPost, "/ether/api/collections", `{"name":"x"}`))
	r.Header.Del("X-Ether-Request")
	if rec := serve(h, r); rec.Code != 403 {
		t.Fatalf("csrf: %d", rec.Code)
	}
}

func TestCollectionsAPIAuthAndPublicImages(t *testing.T) {
	h, _, im := studioHandler(t)
	// Without a token the API is closed…
	if rec := serve(h, req(http.MethodGet, "/ether/api/collections", "")); rec.Code != 401 {
		t.Fatalf("unauthenticated list: %d", rec.Code)
	}
	// …but artwork and TMDB images load from <img> tags.
	rec := serve(h, req(http.MethodGet, "/ether/api/collections/1/artwork?w=300", ""))
	if rec.Code != 200 || rec.Body.String() != "JPEGDATA" || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("Last-Modified") == "" {
		t.Fatalf("artwork: %d %v", rec.Code, rec.Header())
	}
	r := req(http.MethodGet, "/ether/api/collections/1/artwork", "")
	r.Header.Set("If-Modified-Since", rec.Header().Get("Last-Modified"))
	if rec := serve(h, r); rec.Code != 304 {
		t.Fatalf("artwork revalidation: %d", rec.Code)
	}
	if rec := serve(h, req(http.MethodGet, "/ether/api/collections/zzz/artwork", "")); rec.Code != 404 {
		t.Fatalf("missing artwork: %d", rec.Code)
	}
	rec = serve(h, req(http.MethodGet, "/ether/api/image?path=/abc_1-x.jpg&w=342", ""))
	if rec.Code != 200 || rec.Body.String() != "IMG/abc_1-x.jpg" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("image: %d %s %v", rec.Code, rec.Body, rec.Header())
	}
	r = req(http.MethodGet, "/ether/api/image?path=/abc_1-x.jpg&w=342", "")
	r.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	calls := im.calls
	if rec := serve(h, r); rec.Code != 304 || im.calls != calls {
		t.Fatalf("image etag: %d", rec.Code)
	}
	for _, bad := range []string{"", "abc.jpg", "/../../etc/passwd", "/a/b.jpg", "https://evil.example/x.jpg", "/x.php"} {
		if rec := serve(h, req(http.MethodGet, "/ether/api/image?path="+bad, "")); rec.Code != 400 {
			t.Errorf("image path %q: %d", bad, rec.Code)
		}
	}
	if rec := serve(h, req(http.MethodGet, "/ether/api/image?path=/missing.jpg", "")); rec.Code != 404 {
		t.Fatalf("missing image: %d", rec.Code)
	}
}

func TestCatalogHelperAPI(t *testing.T) {
	h, _, _ := studioHandler(t)
	rec := serve(h, authed(req(http.MethodGet, "/ether/api/catalog/search?q=Dark&type=tv&limit=5", "")))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"title":"Dark show"`) || !strings.Contains(rec.Body.String(), `"type":"show"`) {
		t.Fatalf("search: %d %s", rec.Code, rec.Body)
	}
	rec = serve(h, authed(req(http.MethodGet, "/ether/api/catalog/facets?type=movie&facet=language", "")))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `{"values":[{"key":"ko","title":"Korean","count":3}]}`) {
		t.Fatalf("facets: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(h, authed(req(http.MethodGet, "/ether/api/catalog/facets?type=movie", ""))); rec.Code != 400 {
		t.Fatalf("facet missing: %d", rec.Code)
	}
	if rec := serve(h, authed(req(http.MethodGet, "/ether/api/catalog/facets?facet=mood", ""))); rec.Code != 400 {
		t.Fatalf("bad facet: %d", rec.Code)
	}
	rec = serve(h, authed(req(http.MethodGet, "/ether/api/catalog/lookup?kind=person&q=nolan&region=gb", "")))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"image":"/ether/api/image?path=%2Fnolan.jpg\u0026w=185"`) || !strings.Contains(rec.Body.String(), "Directing GB") {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(h, authed(req(http.MethodGet, "/ether/api/catalog/lookup?kind=pizza&q=x", ""))); rec.Code != 400 {
		t.Fatalf("bad lookup kind: %d", rec.Code)
	}
	rec = serve(h, authed(req(http.MethodGet, "/ether/api/catalog/title/"+core.MovieKey(7).String(), "")))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"keywords":["heist"]`) || !strings.Contains(body, `"collection":{"id":10,"name":"Saga"}`) ||
		!strings.Contains(body, `"cast":[{"id":1,"name":"Actor","role":"Lead"`) || !strings.Contains(body, `"directors":[{"id":2`) {
		t.Fatalf("title: %d %s", rec.Code, body)
	}
	if rec := serve(h, authed(req(http.MethodGet, "/ether/api/catalog/title/"+core.MovieKey(8).String(), ""))); rec.Code != 404 {
		t.Fatalf("missing title: %d", rec.Code)
	}
	if rec := serve(h, authed(req(http.MethodGet, "/ether/api/catalog/title/banana", ""))); rec.Code != 404 {
		t.Fatalf("bad key: %d", rec.Code)
	}
}

// listStudio returns a fixed collection list.
type listStudio struct {
	*fakeStudio
	list []core.CollectionSpec
}

func (l listStudio) ListSpecs(ctx context.Context) ([]core.CollectionSpec, error) { return l.list, nil }

// One collection that cannot be encoded (a stamp past year 9999, as
// nanosecond imports produced) is left out; the rest of the list still loads
// (GitHub #180).
func TestCollectionsListSkipsUnencodableCollection(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	st := newFakeStudio()
	bad := time.Unix(1_790_000_000_000_000_000, 0)
	list := []core.CollectionSpec{st.specs["1"], {ID: "7", Name: "Imported", UpdatedAt: &bad}, st.specs["top-rated-movie"]}
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), Collections: listStudio{st, list}, CatalogHelper: fakeHelper{}, Images: &fakeImages{}})
	rec := serve(h, authed(req(http.MethodGet, "/ether/api/collections", "")))
	var got struct {
		Collections []struct {
			ID string `json:"id"`
		} `json:"collections"`
	}
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || len(got.Collections) != 2 || got.Collections[0].ID != "1" || got.Collections[1].ID != "top-rated-movie" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
}
