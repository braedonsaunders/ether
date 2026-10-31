package migrate

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type catalogFake struct{ core.Catalog }

func (catalogFake) Get(_ context.Context, k core.RatingKey) (*core.Title, error) {
	return &core.Title{Key: k, Title: "Example", Year: 2024, Type: core.TypeMovie, IDs: core.IDs{TMDB: 11}}, nil
}
func (catalogFake) Search(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
	return []core.Title{{Key: core.MovieKey(11), Title: "Example", Year: 2024, Type: core.TypeMovie}}, nil
}
func TestPMSDryRunAndIdempotentImport(t *testing.T) {
	ctx := context.Background()
	db, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	_, e = db.Exec(`INSERT INTO auth_users(id,plex_id,username,created_at,updated_at)VALUES(1,99,'viewer',0,0)`)
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Errorf("PMS mutation %s", r.Method)
		}
		if r.Header.Get("X-Plex-Token") != "secret" {
			t.Error("missing access token")
		}
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/identity":
			w.Write([]byte(`<MediaContainer machineIdentifier="fixture"/>`))
		case "/accounts":
			w.Write([]byte(`<MediaContainer><Account id="1" name="viewer"/></MediaContainer>`))
		case "/library/sections":
			w.Write([]byte(`<MediaContainer><Directory key="1" type="movie" title="Movies"/></MediaContainer>`))
		case "/library/sections/1/all":
			w.Write([]byte(`<MediaContainer totalSize="2"><Video ratingKey="a" type="movie" title="Example" viewCount="3" viewOffset="20000" duration="100000" lastViewedAt="1700000000" userRating="8"><Guid id="tmdb://11"/></Video><Video ratingKey="bad" type="movie" title="Unknown" guid="plex://movie/opaque" viewCount="1"/></MediaContainer>`))
		case "/library/sections/1/collections":
			w.Write([]byte(`<MediaContainer><Directory key="5" title="Favourites"/></MediaContainer>`))
		case "/library/collections/5/children", "/playlists/6/items":
			w.Write([]byte(`<MediaContainer><Video ratingKey="a" type="movie" title="Example"><Guid id="tmdb://11"/></Video></MediaContainer>`))
		case "/status/sessions/history/all":
			w.Write([]byte(`<MediaContainer><Video ratingKey="a" type="movie" title="Example" accountID="1" viewedAt="1700000000"/></MediaContainer>`))
		case "/library/metadata/bad":
			w.WriteHeader(404)
		case "/playlists":
			w.Write([]byte(`<MediaContainer><Playlist ratingKey="6" title="My list"/></MediaContainer>`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	s := New(db, catalogFake{})
	p, e := s.Preview(ctx, Request{URL: srv.URL, Token: "secret", Viewers: []Viewer{{LocalUserID: 1, Name: "Viewer"}}})
	if e != nil {
		t.Fatal(e)
	}
	if calls < 6 || len(p.Items) != 1 || len(p.Lists) != 2 || len(p.Warnings) < 1 {
		t.Fatalf("preview %+v calls %d", p, calls)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("preview leaked token")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM state_watch`).Scan(&n)
	if n != 0 {
		t.Fatal("dry run mutated state")
	}
	for i := 0; i < 2; i++ {
		p, e = s.Import(ctx, p.ID)
		if e != nil || !p.Imported {
			t.Fatalf("import %v %v", p, e)
		}
	}
	for _, table := range []string{"state_watch", "state_history", "state_playlists", "collections_specs"} {
		if e = db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); e != nil || n != 1 {
			t.Fatalf("%s count=%d err=%v", table, n, e)
		}
	}
	var count int
	var offset int64
	db.QueryRow(`SELECT view_count,view_offset_ns FROM state_watch`).Scan(&count, &offset)
	if count != 3 || offset != 20000000000 {
		t.Fatalf("state counts %d offset %d", count, offset)
	}
}
func TestLetterboxdCSVImportExport(t *testing.T) {
	ctx := context.Background()
	db, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at)VALUES(1,99,0,0)`)
	s := New(db, catalogFake{})
	p, e := s.PreviewCSV(ctx, 1, "letterboxd", "history", strings.NewReader("Date,Name,Year,Rating,Watched Date\n2024-03-01,Example,2024,4.5,2024-03-01\n"))
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 1 || p.Items[0].Rating != 9 {
		t.Fatalf("plan %+v", p)
	}
	if _, e = s.Import(ctx, p.ID); e != nil {
		t.Fatal(e)
	}
	var out strings.Builder
	if e = s.ExportCSV(ctx, 1, "letterboxd", "history", &out); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "Example,2024,4.5,2024-03-01,11") {
		t.Fatalf("export %s", out.String())
	}
}
func TestRatingAndWatchlistImportPreserveProgress(t *testing.T) {
	ctx := context.Background()
	db, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at)VALUES(1,99,0,0)`)
	key := core.MovieKey(11)
	db.Exec(`INSERT INTO state_watch(user_id,rating_key,view_offset_ns,view_count)VALUES(?,?,123000000000,3)`, 1, key)
	s := New(db, catalogFake{})
	for _, kind := range []string{"ratings", "watchlist"} {
		p, e := s.PreviewCSV(ctx, 1, "letterboxd", kind, strings.NewReader("Name,Year,Rating\nExample,2024,4.5\n"))
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Import(ctx, p.ID); e != nil {
			t.Fatal(e)
		}
	}
	var offset int64
	var count int
	var rating float64
	if e = db.QueryRow(`SELECT view_offset_ns,view_count,user_rating FROM state_watch WHERE user_id=1`).Scan(&offset, &count, &rating); e != nil {
		t.Fatal(e)
	}
	if offset != 123000000000 || count != 3 || rating != 9 {
		t.Fatalf("overwrote local state offset=%d count=%d rating=%f", offset, count, rating)
	}
}

func TestHistoryOnlyScopesAccountPagesAndCachesShowIdentities(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO auth_users(id,plex_id,username,created_at,updated_at)VALUES(1,99,'viewer',0,0)`); err != nil {
		t.Fatal(err)
	}
	parentReads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("source mutation: %s", r.Method)
		}
		switch r.URL.Path {
		case "/identity":
			w.Write([]byte(`<MediaContainer machineIdentifier="history-fixture"/>`))
		case "/accounts":
			w.Write([]byte(`<MediaContainer><Account id="19" name="Viewer"/><Account id="88" name="Friend"/></MediaContainer>`))
		case "/library/sections":
			w.Write([]byte(`<MediaContainer><Directory key="1" type="movie"/><Directory key="2" type="show"/></MediaContainer>`))
		case "/library/sections/1/all", "/library/sections/2/all":
			if r.Header.Get("X-Plex-Container-Size") != "500" || r.Header.Get("X-Plex-Container-Start") != "0" {
				t.Error("missing PMS pagination headers")
			}
			if r.URL.Query().Get("unwatched") != "0" {
				t.Error("history-only import must request watched records")
			}
			if strings.Contains(r.URL.Path, "/1/") {
				w.Write([]byte(`<MediaContainer totalSize="2"><Video ratingKey="film" type="movie" title="Watched film" viewCount="1" lastViewedAt="1700000000"><Guid id="tmdb://11"/></Video><Video ratingKey="unwatched" type="movie" title="Unwatched"><Guid id="tmdb://12"/></Video></MediaContainer>`))
			} else {
				w.Write([]byte(`<MediaContainer totalSize="2"><Video ratingKey="ep1" type="episode" title="First" grandparentRatingKey="show" parentIndex="1" index="1" viewCount="1" lastViewedAt="1700000100"/><Video ratingKey="ep2" type="episode" title="Second" grandparentRatingKey="show" parentIndex="1" index="2" viewCount="1" lastViewedAt="1700000200"/></MediaContainer>`))
			}
		case "/library/metadata/show":
			parentReads++
			w.Write([]byte(`<MediaContainer><Directory ratingKey="show" type="show"><Guid id="tmdb://200"/></Directory></MediaContainer>`))
		case "/status/sessions/history/all":
			if r.URL.Query().Get("accountID") != "19" {
				t.Error("history not scoped to source account")
			}
			w.Write([]byte(`<MediaContainer><Video ratingKey="film" type="movie" title="Watched film" accountID="19" viewedAt="1700000000"/><Video ratingKey="ep1" type="episode" title="First" accountID="19" viewedAt="1700000100"/><Video ratingKey="film" type="movie" title="Friend play" accountID="88" viewedAt="1700000300"/><Video ratingKey="film" type="movie" title="Unattributed play" viewedAt="1700000400"/></MediaContainer>`))
		default:
			t.Errorf("history-only preview fetched unrelated endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	service := New(db, catalogFake{})
	plan, err := service.Preview(ctx, Request{URL: srv.URL, Token: "fixture-token", HistoryOnly: true, Viewers: []Viewer{{LocalUserID: 1, Name: "viewer", PlexID: 99}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 3 || len(plan.History) != 2 || len(plan.Lists) != 0 || parentReads != 1 {
		t.Fatalf("items=%d history=%d lists=%d parentReads=%d", len(plan.Items), len(plan.History), len(plan.Lists), parentReads)
	}
	if plan.Viewers[0].SourceAccountID != 19 {
		t.Fatal("used a destination or plex.tv ID as the source account")
	}
	if plan.Items[1].Key != core.EpisodeKey(200, 1, 1) || plan.Items[2].Key != core.EpisodeKey(200, 1, 2) {
		t.Fatal("episode identity mapping changed")
	}
	if _, err = service.Import(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM state_history WHERE user_id=1`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("history count=%d err=%v", count, err)
	}
}

func TestPreviewSkipsUnwatchedAndMatchesPlexGUIDByTitleYear(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO auth_users(id,plex_id,username,created_at,updated_at)VALUES(1,99,'viewer',0,0)`); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/identity":
			w.Write([]byte(`<MediaContainer machineIdentifier="guid-fallback"/>`))
		case "/accounts":
			w.Write([]byte(`<MediaContainer><Account id="1" name="viewer"/></MediaContainer>`))
		case "/library/sections":
			w.Write([]byte(`<MediaContainer><Directory key="1" type="movie" title="Movies"/></MediaContainer>`))
		case "/library/sections/1/all":
			w.Write([]byte(`<MediaContainer totalSize="3"><Video ratingKey="watched-plex" type="movie" title="Example" year="2024" guid="plex://movie/opaque" viewCount="1" lastViewedAt="1700000000"/><Video ratingKey="unwatched-opaque" type="movie" title="Never Watched" guid="plex://movie/unwatched" viewCount="0"/><Video ratingKey="collection-item" type="movie" title="Example" year="2024" guid="plex://movie/col" viewCount="0"/></MediaContainer>`))
		case "/library/metadata/watched-plex", "/library/metadata/collection-item":
			w.Write([]byte(`<MediaContainer><Video ratingKey="watched-plex" type="movie" title="Example" year="2024" guid="plex://movie/opaque"/></MediaContainer>`))
		case "/library/sections/1/collections":
			w.Write([]byte(`<MediaContainer><Directory key="5" title="Picks"/></MediaContainer>`))
		case "/library/collections/5/children":
			w.Write([]byte(`<MediaContainer><Video ratingKey="collection-item" type="movie" title="Example" year="2024" guid="plex://movie/col"/></MediaContainer>`))
		case "/status/sessions/history/all", "/playlists":
			w.Write([]byte(`<MediaContainer totalSize="0"/>`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	svc := New(db, catalogFake{})
	plan, err := svc.Preview(ctx, Request{URL: srv.URL, Token: "secret", Viewers: []Viewer{{LocalUserID: 1, Name: "viewer"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Key != core.MovieKey(11) {
		t.Fatalf("items=%+v", plan.Items)
	}
	for _, warn := range plan.Warnings {
		if strings.Contains(warn, "Unmatched") {
			t.Fatalf("unexpected unmatched warning: %s", warn)
		}
	}
	if len(plan.Lists) != 1 || len(plan.Lists[0].Keys) != 1 || plan.Lists[0].Keys[0] != core.MovieKey(11) {
		t.Fatalf("lists=%+v", plan.Lists)
	}
}
