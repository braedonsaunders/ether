package vela

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

// Unknown membership and known-empty membership have different wire meanings;
// overlapping guides preserve their owner order, stable IDs, and provider group.
func TestLiveOwnerGuides(t *testing.T) {
	source := &livePageFake{channels: []frontend.LiveChannel{
		{ID: "unknown", Name: "Unknown"},
		{ID: "empty", Name: "Empty", Guides: []frontend.LiveGuide{}},
		{ID: "overlap", Name: "Overlap", Group: "Provider news", Guides: []frontend.LiveGuide{{ID: "family", Name: "Family"}, {ID: "news", Name: "News"}}},
	}}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Identity: identityFake{}, Experience: frontendtest.Experience{}, LiveTV: source}}
	fetch := func() (*httptest.ResponseRecorder, []map[string]json.RawMessage) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/vela/v0/live/channels?limit=100", nil)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", "device")
		w := httptest.NewRecorder()
		i.channels(w, r)
		var page struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || len(page.Items) != 3 {
			t.Fatalf("page: %d %s", w.Code, w.Body.String())
		}
		return w, page.Items
	}
	first, items := fetch()
	if _, ok := items[0]["guides"]; ok {
		t.Fatal("unknown guides advertised")
	}
	if string(items[1]["guides"]) != "[]" {
		t.Fatalf("empty guides: %s", items[1]["guides"])
	}
	var guides []struct{ ID, Name string }
	if err := json.Unmarshal(items[2]["guides"], &guides); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(guides, []struct{ ID, Name string }{{"family", "Family"}, {"news", "News"}}) || string(items[2]["group"]) != `"Provider news"` {
		t.Fatalf("memberships: %s group=%s", items[2]["guides"], items[2]["group"])
	}
	source.channels[2].Guides[0].Name = "Renamed"
	next, items := fetch()
	if first.Header().Get("ETag") == next.Header().Get("ETag") {
		t.Fatal("guide rename did not invalidate channel cache")
	}
	if err := json.Unmarshal(items[2]["guides"], &guides); err != nil || guides[0].ID != "family" || guides[0].Name != "Renamed" {
		t.Fatalf("rename: %+v %v", guides, err)
	}
}
