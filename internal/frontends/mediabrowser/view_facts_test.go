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
	var libraryCalls atomic.Int32
	var blockLibrary atomic.Bool
	libraryBlock := make(chan struct{})
	rowBlock := make(chan struct{})
	title := frontend.Title{Key: 4, Type: "show", Title: "Shown", Released: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)}
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			libraryCalls.Add(1)
			if blockLibrary.Load() {
				<-libraryBlock
			}
			return frontend.Page{Items: []frontend.Title{title}, Total: 27000}, nil
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
	warm := call()
	if warm.Code != 200 {
		t.Fatal(warm.Code, warm.Body.String())
	}
	var listed struct{ Items []map[string]any }
	if json.Unmarshal(warm.Body.Bytes(), &listed) != nil || len(listed.Items) == 0 || listed.Items[len(listed.Items)-1]["ChildCount"] != float64(27000) {
		t.Fatal(warm.Body.String())
	}
	primed := libraryCalls.Load()
	if primed == 0 {
		t.Fatal("warm view list did not count the library")
	}
	blockLibrary.Store(true)
	start := time.Now()
	again := call()
	elapsed := time.Since(start)
	if again.Code != 200 || elapsed > 100*time.Millisecond {
		t.Fatalf("warm UserViews took %s: %d %s", elapsed, again.Code, again.Body.String())
	}
	if libraryCalls.Load() != primed {
		t.Fatalf("warm UserViews walked the library again: %d calls", libraryCalls.Load())
	}
	var listedAgain struct{ Items []map[string]any }
	if json.Unmarshal(again.Body.Bytes(), &listedAgain) != nil || listedAgain.Items[len(listedAgain.Items)-1]["ChildCount"] != float64(27000) {
		t.Fatal(again.Body.String())
	}
	close(rowBlock)
	close(libraryBlock)
}
