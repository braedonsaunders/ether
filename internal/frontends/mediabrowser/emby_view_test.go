package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestEmbyViewReportsUnsupportedMetadataLocking(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
		Experience: frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		}},
	}
	inst, err := (Emby{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/UserViews", nil)
	r.Header.Set("X-Emby-Token", "grant")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	var response struct{ Items []map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	view := response.Items[0]
	if view["LockData"] != false || len(view["LockedFields"].([]any)) != 0 {
		t.Fatal(view)
	}
	if _, ok := view["ImageTags"]; ok {
		t.Fatal("unknown view artwork was invented", view)
	}
}
