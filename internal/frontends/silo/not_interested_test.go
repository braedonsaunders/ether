package silo

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestNotInterestedListsAuthorizedEntriesAndRealTimestamp(t *testing.T) {
	v := &frontend.Viewer{ID: 7}
	key, hidden := frontend.RatingKey(1000000000000001), frontend.RatingKey(1000000000000002)
	marked := time.Date(2024, 3, 2, 1, 2, 3, 0, time.UTC)
	entry := frontend.NotInterestedEntry{Key: key, AddedAt: marked}
	var query frontend.PageQuery
	experience := frontendtest.Experience{
		NotInterestedFunc: func(_ context.Context, viewer *frontend.Viewer, _ frontend.Device, q frontend.PageQuery) (frontend.NotInterestedPage, error) {
			if viewer != v {
				t.Fatal("wrong viewer")
			}
			query = q
			return frontend.NotInterestedPage{Items: []frontend.NotInterestedEntry{entry}, Total: 2}, nil
		},
		NotInterestedItemFunc: func(_ context.Context, viewer *frontend.Viewer, _ frontend.Device, item frontend.RatingKey) (frontend.NotInterestedEntry, error) {
			if viewer != v || item != key {
				return frontend.NotInterestedEntry{}, errors.New("hidden")
			}
			return entry, nil
		},
		ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
			if req.Viewer != v || req.Key != key {
				return frontend.ItemDetail{}, errors.New("hidden")
			}
			return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: "movie", Title: "Film"}}, nil
		},
	}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}, Experience: experience}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	path := "/api/v2/not-interested/" + ids.RatingKeyGUID(key)
	w := call(path)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"marked_at":"2024-03-02T01:02:03Z"`) {
		t.Fatalf("entry: %d %s", w.Code, w.Body.String())
	}
	list := call("/api/v2/not-interested?limit=1&cursor=0")
	if list.Code != 200 || query.Limit != 1 || query.Offset != 0 || !strings.Contains(list.Body.String(), `"next_cursor":"1"`) || !strings.Contains(list.Body.String(), `"not_interested":true`) {
		t.Fatalf("list: %d %s %+v", list.Code, list.Body.String(), query)
	}
	if w := call("/api/v2/not-interested/" + ids.RatingKeyGUID(hidden)); w.Code != 404 {
		t.Fatalf("hidden entry: %d", w.Code)
	}
	entry.AddedAt = time.Time{}
	if w := call(path); w.Code != 503 {
		t.Fatalf("missing timestamp must not be fabricated: %d %s", w.Code, w.Body.String())
	}
	entry.AddedAt = marked
	if w := call("/api/v2/not-interested?limit=201"); w.Code != 400 {
		t.Fatalf("limit: %d", w.Code)
	}
	if w := call("/api/v2/capabilities/not-interested"); w.Code != 200 || !strings.Contains(w.Body.String(), `"allowed":true`) {
		t.Fatalf("capability: %d %s", w.Code, w.Body.String())
	}
	if data, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", ""); e == nil {
		doc, e := openapi.Parse(data)
		if e != nil {
			t.Fatal(e)
		}
		for _, test := range []struct {
			path     string
			response *httptest.ResponseRecorder
		}{{path, w}, {"/api/v2/not-interested", list}} {
			if e = doc.Validate("GET", test.path, test.response.Code, test.response.Header().Get("Content-Type"), test.response.Body.Bytes()); e != nil {
				t.Errorf("%s spec: %v", test.path, e)
			}
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
}
