package mediabrowser

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestUserViewsOnProductionCopy(t *testing.T) {
	const path = "/tmp/ether-userviews.db"
	if _, err := os.Stat(path); err != nil {
		t.Skip(err)
	}
	db, err := sql.Open("sqlite-cached", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(4)
	cat, err := catalog.New(db, nil, config.Catalog{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}, {ID: "2", Name: "Shows", Type: "show"}}, nil
		},
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			t.Error("UserViews browsed the library")
			return frontend.Page{}, nil
		},
		LibraryCountFunc: func(ctx context.Context, _ *frontend.Viewer, id string) (int, error) {
			kind := core.TypeMovie
			if id == "2" {
				kind = core.TypeShow
			}
			return cat.CountBrowsable(ctx, kind)
		},
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			return nil, nil
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token == "prismtest" {
			v := frontend.Viewer{ID: 2, Username: "prismtest", MaxContentRating: "PG-13"}
			return &v, nil
		}
		v := frontend.Viewer{ID: 1, Username: "owner", IsOwner: true}
		return &v, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: ident, Experience: exp, Sessions: frontendtest.Sessions{},
		PageFreshness: func() frontend.PageFreshness { return frontend.PageFreshness{Catalog: cat.WriteGeneration()} },
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(token string) (time.Duration, int) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/UserViews", nil)
		r.Header.Set("X-Emby-Token", token)
		r.Header.Set("X-Emby-Device-Id", token)
		start := time.Now()
		inst.Handler().ServeHTTP(w, r)
		elapsed := time.Since(start)
		if w.Code != 200 {
			t.Fatalf("%s UserViews %d %s", token, w.Code, w.Body.String())
		}
		return elapsed, w.Body.Len()
	}
	pair := func(label string) {
		t.Helper()
		var wg sync.WaitGroup
		var owner, other time.Duration
		wg.Add(2)
		go func() {
			defer wg.Done()
			owner, _ = call("owner")
		}()
		go func() {
			defer wg.Done()
			other, _ = call("prismtest")
		}()
		start := time.Now()
		wg.Wait()
		wall := time.Since(start)
		t.Logf("%s owner %s prismtest %s wall %s", label, owner, other, wall)
		if owner > 200*time.Millisecond || other > 200*time.Millisecond || wall > 200*time.Millisecond {
			t.Fatalf("%s exceeded 200ms: owner %s prismtest %s wall %s", label, owner, other, wall)
		}
	}
	pair("cold")
	pair("warm")
	plan, err := db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN SELECT COUNT(*) FROM catalog_titles WHERE type='movie' AND browsable=1 AND adult=0`)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Logf("movie count plan: %s", detail)
	}
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
}
