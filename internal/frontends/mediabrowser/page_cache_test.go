package mediabrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestBrowsePageCacheFollowsFreshness(t *testing.T) {
	name := "First"
	var gen uint64
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie", Title: name}}, nil
	}}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		return &frontend.Viewer{ID: 1}, nil
	}}
	inst, err := (Jellyfin{MachineID: "cache"}).Mount(context.Background(), frontend.Host{
		Identity: identity, Experience: experience,
		PageFreshness: func() frontend.PageFreshness { return frontend.PageFreshness{Catalog: gen} },
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/Items/" + ids.RatingKeyGUID(5)
	get := func(match string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		if match != "" {
			r.Header.Set("If-None-Match", match)
		}
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	first := get("")
	if first.Code != 200 || !strings.Contains(first.Body.String(), "First") || first.Header().Get("ETag") == "" {
		t.Fatal(first.Code, first.Header().Get("ETag"), first.Body.String())
	}
	name = "Second"
	cached := get("")
	if cached.Code != 200 || !strings.Contains(cached.Body.String(), "First") {
		t.Fatal(cached.Code, cached.Body.String())
	}
	etag := cached.Header().Get("ETag")
	notModified := get(etag)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatal(notModified.Code, notModified.Body.String())
	}
	gen++
	fresh := get("")
	if fresh.Code != 200 || !strings.Contains(fresh.Body.String(), "Second") {
		t.Fatal(fresh.Code, fresh.Body.String())
	}
}
