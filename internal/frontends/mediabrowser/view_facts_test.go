package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestUserViewsStayFastWhileRowsFillALargeLibrary(t *testing.T) {
	var counts atomic.Int32
	rowBlock := make(chan struct{})
	title := frontend.Title{Key: 4, Type: "show", Title: "Shown"}
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			t.Error("UserViews browsed the library")
			return frontend.Page{}, nil
		},
		LibraryCountFunc: func(context.Context, *frontend.Viewer, string) (int, error) {
			counts.Add(1)
			return 27000, nil
		},
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			<-rowBlock
			return []frontend.Row{{ID: "home.trending", Title: "Trending", Kind: "trending", Purpose: frontend.RowRecommendation, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &title}}}}, nil
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
		v := frontend.Viewer{ID: 1, Username: "ada", IsOwner: true}
		return &v, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: ident, Experience: exp, Sessions: frontendtest.Sessions{},
		PageFreshness: func() frontend.PageFreshness { return frontend.PageFreshness{Catalog: 7} },
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func() *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/UserViews", nil)
		r.Header.Set("X-Emby-Token", "grant")
		r.Header.Set("X-Emby-Device-Id", "living-room")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	start := time.Now()
	cold := call()
	if cold.Code != 200 || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("cold UserViews took %s: %d %s", time.Since(start), cold.Code, cold.Body.String())
	}
	var listed struct{ Items []map[string]any }
	if json.Unmarshal(cold.Body.Bytes(), &listed) != nil || len(listed.Items) == 0 || listed.Items[len(listed.Items)-1]["ChildCount"] != float64(27000) {
		t.Fatal(cold.Body.String())
	}
	if _, ok := listed.Items[len(listed.Items)-1]["DateCreated"]; ok {
		t.Fatal("cold view invented DateCreated")
	}
	start = time.Now()
	warm := call()
	if warm.Code != 200 || time.Since(start) > 100*time.Millisecond || counts.Load() != 1 {
		t.Fatalf("warm UserViews took %s after %d counts: %s", time.Since(start), counts.Load(), warm.Body.String())
	}
	close(rowBlock)
}
