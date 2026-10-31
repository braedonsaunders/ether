package silo

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestSyntheticVersionDateUsesCanonicalTitleDate(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	key := frontend.RatingKey(1000000000000001)
	released := time.Date(2004, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: "movie", Released: released, UpdatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}, Versions: []frontend.Version{{ID: "cut"}}}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, Experience: exp, Sessions: frontendtest.Sessions{}, SourceOffers: testSourceOffers{}, VersionHandles: testVersionHandles{}, Capabilities: frontendtest.Capabilities{}, IDs: ids.Codecs{Allocator: fileIDs{}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, known := range []bool{true, false, false} {
		if !known {
			if released.IsZero() {
				released = time.Now().Add(24 * time.Hour)
			} else {
				released = time.Time{}
			}
		}
		r := httptest.NewRequest("GET", "/api/v2/catalog/items/"+ids.RatingKeyGUID(key), nil)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("date unavailable broke browsing: %d %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		versions := body["versions"].([]any)
		if len(versions) != 1 {
			t.Fatal(body)
		}
		version := versions[0].(map[string]any)
		if known && version["added_at"] != "2004-01-01T00:00:00.000Z" {
			t.Fatalf("catalogue time used: %v", version)
		}
		if !known {
			if _, ok := version["added_at"]; ok {
				t.Fatalf("unknown/future date invented: %v", version)
			}
		}
	}
}
