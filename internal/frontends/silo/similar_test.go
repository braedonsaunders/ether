package silo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestConnectionMonitorHealthMatchesIdentity(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7, Username: "owner"}
	inst, err := (Frontend{MachineID: "lab"}).Mount(context.Background(), frontend.Host{
		Identity: testDeviceIdentity(frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, viewer),
		Config:   func() frontend.Settings { return frontend.Settings{"serverName": "Mac Prism Lab"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	healthReq.Header.Set("Accept", "application/json")
	health := httptest.NewRecorder()
	inst.Handler().ServeHTTP(health, healthReq)
	var body map[string]any
	if health.Code != 200 || json.Unmarshal(health.Body.Bytes(), &body) != nil || body["status"] != "ok" || body["server_name"] != "Mac Prism Lab" || body["server_id"] == "" {
		t.Fatalf("health %d %s", health.Code, health.Body.String())
	}
	identityReq := httptest.NewRequest(http.MethodGet, "/api/v2/system/identity", nil)
	identity := httptest.NewRecorder()
	inst.Handler().ServeHTTP(identity, identityReq)
	var id map[string]any
	if identity.Code != 200 || json.Unmarshal(identity.Body.Bytes(), &id) != nil || id["server_id"] != body["server_id"] {
		t.Fatalf("identity %d %s health id %v", identity.Code, identity.Body.String(), body["server_id"])
	}
}

func TestSimilarUsesIndexedRelatedTitles(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	source := frontend.RatingKey(1000000000000001)
	related := frontend.Title{Key: frontend.RatingKey(1000000000000002), Type: "movie", Title: "Sintel"}
	extra := frontend.Title{Key: frontend.RatingKey(1000000000000003), Type: "movie", Title: "Bunny"}
	var gotLimit, gotOffset int
	exp := frontendtest.Experience{
		SimilarFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, key frontend.RatingKey, q frontend.ItemQuery) (frontend.SearchPage, error) {
			gotLimit, gotOffset = q.Limit, q.Offset
			if v != viewer || key != source {
				t.Fatalf("scope %+v %v", v, key)
			}
			if key == 0 {
				return frontend.SearchPage{}, frontend.ErrNotFound
			}
			items := []frontend.Title{related, extra}
			if q.Offset >= len(items) {
				return frontend.SearchPage{Page: core.Page{Items: []frontend.Title{}, Total: len(items)}}, nil
			}
			end := len(items)
			more := frontend.Continuation("")
			if q.Limit > 0 && q.Offset+q.Limit < end {
				end = q.Offset + q.Limit
				more = frontend.ContinuationMore
			}
			return frontend.SearchPage{Page: core.Page{Items: items[q.Offset:end], Total: len(items)}, Continuation: more}, nil
		},
	}
	inst, err := (Frontend{MachineID: "lab"}).Mount(context.Background(), frontend.Host{
		Identity:   testDeviceIdentity(frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, viewer),
		Experience: exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	id := ids.RatingKeyGUID(source)
	page := call("/api/v2/recommendations/similar/" + id + "?backdrop_width=2688&limit=12&poster_width=440")
	if page.Code != 200 || gotLimit != 12 || gotOffset != 0 || !strings.Contains(page.Body.String(), `"title":"Sintel"`) || !strings.Contains(page.Body.String(), `"content_id":"`+ids.RatingKeyGUID(related.Key)+`"`) || strings.Contains(page.Body.String(), `"page"`) {
		t.Fatalf("similar %d limit=%d %s", page.Code, gotLimit, page.Body.String())
	}
	bounded := call("/api/v2/recommendations/similar/" + id + "?limit=1")
	if bounded.Code != 200 || !strings.Contains(bounded.Body.String(), `"has_more":true`) || !strings.Contains(bounded.Body.String(), `"next_cursor":"1"`) || strings.Contains(bounded.Body.String(), "Bunny") {
		t.Fatalf("bounded %d %s", bounded.Code, bounded.Body.String())
	}
	if w := call("/api/v2/recommendations/similar/" + id + "?limit=51"); w.Code != 400 {
		t.Fatalf("limit %d", w.Code)
	}
	if w := call("/api/v2/recommendations/similar/not-an-id"); w.Code != 404 {
		t.Fatalf("id %d", w.Code)
	}
	missing := httptest.NewRequest(http.MethodGet, "/api/v2/recommendations/similar/"+id, nil)
	unauth := httptest.NewRecorder()
	inst.Handler().ServeHTTP(unauth, missing)
	if unauth.Code != 401 {
		t.Fatalf("auth %d", unauth.Code)
	}
}
