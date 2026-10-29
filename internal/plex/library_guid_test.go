package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLibraryGUIDLookup(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const guid = "plex://movie/5d7768265af944001f1f6976"
	if _, err = db.Exec(`INSERT INTO plexguid_mappings VALUES(?,?,?)`, int64(fightClub.Key), guid, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	const showGUID = "plex://show/5d9c086c46115600200aa2fe"
	if _, err = db.Exec(`INSERT INTO plexguid_mappings VALUES(?,?,?)`, int64(breakingBad.Key), showGUID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	pg := plexguid.New(db, plexguid.Options{})
	defer pg.Close()
	f := newFixture(func(d *Deps) { d.PlexGUID = pg })
	defer f.srv.Close()
	f.srv.d.Catalog = guidCatalog{f.catalog}
	for _, tc := range []struct {
		path string
		want []core.RatingKey
	}{
		{"/library/all?guid=" + url.QueryEscape(guid), []core.RatingKey{fightClub.Key}},
		{"/library/all?type=1&guid=" + url.QueryEscape(guid), []core.RatingKey{fightClub.Key}},
		{"/library/sections/1/all?guid=" + url.QueryEscape(guid), []core.RatingKey{fightClub.Key}},
		{"/library/sections/2/all?guid=" + url.QueryEscape(guid), nil},
		{"/library/all?type=2&guid=" + url.QueryEscape(guid), nil},
		{"/library/sections/1/all?guid=invalid", nil},
		{"/library/sections/1/all?guid=", nil},
		{"/library/all?guid=tmdb://550&type=1", []core.RatingKey{fightClub.Key}},
		{"/library/all?guid=imdb://tt0137523", []core.RatingKey{fightClub.Key}},
		{"/library/all?guid=" + url.QueryEscape(showGUID), []core.RatingKey{breakingBad.Key}},
		{"/library/sections/2/all?guid=" + url.QueryEscape(showGUID), []core.RatingKey{breakingBad.Key}},
		{"/library/all?type=invalid&guid=" + url.QueryEscape(guid), nil},
		{"/library/all?guid=tmdb://-1&type=1", nil},
		{"/library/all?guid=tvdb://81189", []core.RatingKey{breakingBad.Key}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := f.get(t, tc.path, jsonAccept...)
			var body struct {
				MediaContainer struct {
					Metadata []struct {
						RatingKey string `json:"ratingKey"`
					}
				}
			}
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
				t.Fatalf("status/body: %d %s", rec.Code, rec.Body.String())
			}
			got := body.MediaContainer.Metadata
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i, k := range tc.want {
				if got[i].RatingKey != k.String() {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

// The standard fixture deliberately does not implement external lookups.
type guidCatalog struct{ *fakeCatalog }

func (c guidCatalog) ByExternalID(ctx context.Context, source, id string) (*core.Title, error) {
	if source == "imdb" && id == "tt0137523" {
		return c.Get(ctx, fightClub.Key)
	}
	if source == "tvdb" && id == "81189" {
		return c.Get(ctx, breakingBad.Key)
	}
	return nil, core.ErrNotFound
}

func TestLibraryGUIDVisibilityAndPaging(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.Auth.(*fakeAuth).users["kid"] = core.User{ID: 30, MaxContentRating: "PG"}
	f.srv.d.Auth.(*fakeAuth).users["tvonly"] = core.User{ID: 31, AllowedSections: []int{2}}
	for _, token := range []string{"kid", "tvonly"} {
		for _, path := range []string{"/library/all?guid=tmdb://550&type=1", "/library/sections/1/all?guid=tmdb://550"} {
			rec := f.get(t, path, "Accept", "application/json", "X-Plex-Token", token)
			if (rec.Code != 200 && rec.Code != 404) || strings.Contains(rec.Body.String(), fightClub.Key.String()) {
				t.Fatalf("policy leak for %s: %d %s", token, rec.Code, rec.Body.String())
			}
		}
	}
	for _, mode := range []string{"adult", "future", "availability"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			title := fightClub
			switch mode {
			case "adult":
				title.Adult = true
			case "future":
				title.Released = fixedNow.Add(time.Hour)
			case "availability":
				f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
			}
			f.catalog.titles[title.Key] = title
			rec := f.get(t, "/library/all?guid=tmdb://550&type=1", jsonAccept...)
			if rec.Code != 200 || strings.Contains(rec.Body.String(), title.Key.String()) {
				t.Fatalf("%s leaked %s", mode, rec.Body.String())
			}
		})
	}
	// Repeated identities deduplicate before paging. The omitted type may find
	// both movie and show IDs, but only actual catalogue items survive.
	path := "/library/all?guid=tmdb://550,tmdb://348&guid=tmdb://550&X-Plex-Container-Start=1&X-Plex-Container-Size=1"
	rec := f.get(t, path, jsonAccept...)
	var body struct {
		MediaContainer struct {
			Size, TotalSize, Offset int
			Metadata                []struct{ RatingKey string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || body.MediaContainer.Size != 1 || body.MediaContainer.TotalSize != 2 || body.MediaContainer.Offset != 1 || body.MediaContainer.Metadata[0].RatingKey != alien.Key.String() {
		t.Fatalf("page: %s", rec.Body.String())
	}
	rec = f.get(t, "/library/all?guid=tmdb://550", "Accept", "application/xml")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `ratingKey="`+fightClub.Key.String()+`"`) {
		t.Fatalf("XML: %s", rec.Body.String())
	}
	rec = f.get(t, "/library/all?guid=tmdb://550", "X-Plex-Token", "invalid")
	if rec.Code != 401 {
		t.Fatalf("unauthenticated lookup=%d", rec.Code)
	}
	rec = f.get(t, "/library/all?guid="+strings.Repeat("tmdb://550,", 21))
	if rec.Code != 400 {
		t.Fatalf("unbounded lookup=%d", rec.Code)
	}
}

func TestLibraryGUIDColdLookup(t *testing.T) {
	const guid = "plex://movie/5d7768265af944001f1f6976"
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/library/metadata/5d7768265af944001f1f6976" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"guid":%q,"type":"movie","Guid":[{"id":"tmdb://550"}]}]}}`, guid)
	}))
	defer upstream.Close()
	pg := plexguid.New(db, plexguid.Options{BaseURL: upstream.URL, Token: func() string { return "fixture" }})
	defer pg.Close()
	f := newFixture(func(d *Deps) { d.PlexGUID = pg })
	defer f.srv.Close()
	for i := 0; i < 2; i++ {
		rec := f.get(t, "/library/all?guid="+url.QueryEscape(guid), jsonAccept...)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), fightClub.Key.String()) || !strings.Contains(rec.Body.String(), guid) || strings.Contains(rec.Body.String(), alien.Key.String()) {
			t.Fatalf("cold lookup: %s", rec.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("cache missed: %d", calls.Load())
	}
	rec := f.get(t, "/library/sections/1/all?guid=plex://movie/5d7768265af944001f1f6977", jsonAccept...)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), fightClub.Key.String()) {
		t.Fatalf("unknown provider identity: %d %s", rec.Code, rec.Body.String())
	}
}
