package vela

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestVelaLibrariesPolicyAndPaging(t *testing.T) {
	views := []frontend.View{{ID: "23", Name: "Owner filter", Type: frontend.TypeMovie, Revision: "one"}, {ID: "71", Name: "Plugin shows", Type: frontend.TypeShow, PluginID: "plugin", KindKey: "series"}}
	calls := 0
	exp := frontendtest.Experience{
		ViewsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.View, error) {
			if v.ID != 1 {
				t.Fatal("viewer identity lost")
			}
			return views, nil
		},
		LibraryItemsFunc: func(_ context.Context, v *frontend.Viewer, d frontend.Device, id string, q frontend.ItemQuery) (frontend.Page, error) {
			calls++
			if v.ID != 1 || d.ID != "device" || id != "23" || !q.NoTotal {
				t.Fatal("unscoped/counting library browse", id, q)
			}
			if q.Sort != frontend.SortTitle || q.Filter.Years[0] != 2025 || q.Watched == nil || *q.Watched {
				t.Fatal("query not preserved", q)
			}
			all := []frontend.Title{{Key: core.MovieKey(1), Title: "A"}, {Key: core.MovieKey(2), Title: "B"}, {Key: core.MovieKey(3), Title: "C"}}
			start := min(q.Offset, len(all))
			end := min(start+q.Limit, len(all))
			return frontend.Page{Items: all[start:end]}, nil
		},
	}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Identity: identityFake{}, Experience: exp}}
	request := func(target, id string, items bool) (int, object) {
		r := httptest.NewRequest("GET", target, nil)
		r.SetPathValue("id", id)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", "device")
		w := httptest.NewRecorder()
		if items {
			i.libraryItems(w, r)
		} else {
			i.libraries(w, r)
		}
		var out object
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return w.Code, out
	}
	status, list := request("/vela/v0/libraries?limit=1", "", false)
	if status != 200 || list["continuation"] != "more" {
		t.Fatal(status, list)
	}
	cursor := list["nextCursor"].(string)
	status, list = request("/vela/v0/libraries?limit=1&cursor="+url.QueryEscape(cursor), "", false)
	if status != 200 || list["items"].([]any)[0].(map[string]any)["id"] != "71" {
		t.Fatal(status, list)
	}
	views[0].Revision = "two"
	if status, _ := request("/vela/v0/libraries?limit=1&cursor="+url.QueryEscape(cursor), "", false); status != 409 {
		t.Fatal("changed library definition reused cursor", status)
	}
	status, page := request("/vela/v0/libraries/23/items?limit=2&year=2025&played=false", "23", true)
	if status != 200 || page["continuation"] != "more" || calls != 2 {
		t.Fatal("bounded lookahead failed", status, page, calls)
	}
	entries := page["items"].([]any)
	if entries[0].(map[string]any)["receiptToken"] != "" {
		t.Fatal("invented attribution")
	}
	next := page["nextCursor"].(string)
	status, page = request("/vela/v0/libraries/23/items?limit=2&year=2025&played=false&cursor="+url.QueryEscape(next), "23", true)
	if status != 200 || page["continuation"] != "exhausted" || len(page["items"].([]any)) != 1 {
		t.Fatal(status, page)
	}
	for _, target := range []struct {
		url, id string
		want    int
	}{{"/vela/v0/libraries/hidden/items", "hidden", 404}, {"/vela/v0/libraries/71/items?sort=title", "71", 400}, {"/vela/v0/libraries/23/items?year=2026&cursor=" + url.QueryEscape(next), "23", 409}} {
		before := calls
		if status, _ := request(target.url, target.id, true); status != target.want || calls != before {
			t.Fatal("query/visibility cursor fence failed", target, status, calls, before)
		}
	}
}
