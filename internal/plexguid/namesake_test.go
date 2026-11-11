package plexguid

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A legacy mapping (stored before identity records existed) on a title the
// catalogue says has a namesake must not be served: the guid may name the
// other work. The read path withholds it and enqueues it for priority repair,
// which drops it when its canonical record turns out to be the namesake. A
// legacy mapping on a namesake-free title keeps serving. This is the v0.3.9
// fix for #430 on upgraded servers: the 1962 "Runner" identity is no longer
// handed to Runner (2026) whose catalogue has a "The Runner" (2026) namesake.
func TestLegacyNamesakeRowWithheldAndRepaired(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	defer db.Close()

	// A real catalogue with the indexed Namesakes lookup, so the test proves
	// the wiring, not a stub.
	cat, err := catalog.New(db, nil, config.Catalog{}, slog.New(slog.DiscardHandler),
		catalog.WithNow(func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }))
	if err != nil {
		t.Fatal(err)
	}
	seedCatalogTitle(t, db, core.MovieKey(770), "movie", "Runner", 2026)
	seedCatalogTitle(t, db, core.MovieKey(771), "movie", "The Runner", 2026) // namesake of 770
	seedCatalogTitle(t, db, core.MovieKey(550), "movie", "Fight Club", 1999) // no namesake

	const (
		runnerBad = "plex://movie/5d7768265af944001f1f6962" // the 1962 "Runner" guid
		fightGood = "plex://movie/5d7768265af944001f1f6976"
	)
	now := time.Now().Unix()
	// Legacy rows: mappings with no plexguid_identity or plexguid_verified.
	if _, err = db.Exec(`INSERT INTO plexguid_mappings(rating_key,guid,checked_at) VALUES(?,?,?), (?,?,?)`,
		int64(core.MovieKey(770)), runnerBad, now, int64(core.MovieKey(550)), fightGood, now); err != nil {
		t.Fatal(err)
	}

	s := New(db, Options{
		Background: true,
		Token:      func() string { return "fixture" },
		Namesakes:  cat.Namesakes,
	})
	defer s.Close()

	// The namesake legacy row is withheld on every read path.
	if got := s.Cached(ctx, core.MovieKey(770)); got != "" {
		t.Fatalf("namesake legacy row served by Cached: %q", got)
	}
	if got := s.CachedMany(ctx, []core.RatingKey{core.MovieKey(770)})[core.MovieKey(770)]; got != "" {
		t.Fatalf("namesake legacy row served by CachedMany: %q", got)
	}
	if _, err := s.ExactKey(ctx, runnerBad); err == nil {
		t.Fatal("namesake legacy row served by ExactKey")
	}
	// VerifiedRange (the plexwatch import path) must not hand the wrong guid
	// to the wrong title either.
	verified, err := s.VerifiedRange(ctx, 0, core.MovieKey(1_000_000), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range verified {
		if v.Key == core.MovieKey(770) {
			t.Fatalf("namesake legacy row served by VerifiedRange: %q", v.GUID)
		}
	}
	// The namesake-free legacy row keeps serving.
	if got := s.Cached(ctx, core.MovieKey(550)); got != fightGood {
		t.Fatalf("namesake-free legacy row withheld: %q", got)
	}

	// The withheld key is enqueued for priority repair.
	if !s.prioQueued[core.MovieKey(770)] && len(s.priority) == 0 {
		t.Fatal("withheld namesake key not enqueued for priority repair")
	}

	// The canonical record for the bad guid is the 1962 namesake, so repair
	// drops the mapping rather than granting it an identity record.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/matches":
			// No search hit proves identity; verification must use the record.
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
		case "/library/metadata/" + runnerBad[len(runnerBad)-24:]:
			fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"guid":%q,"type":"movie","title":"Runner","year":1962,"Guid":[{"id":"tmdb://770"}]}]}}`, runnerBad)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s.o.BaseURL = srv.URL
	if _, err := s.reverify(ctx, core.MovieKey(770)); err != nil {
		t.Fatal(err)
	}
	if got := s.Cached(ctx, core.MovieKey(770)); got != "" {
		t.Fatalf("wrong legacy identity survived repair: %q", got)
	}
}

func seedCatalogTitle(t *testing.T, db *sql.DB, key core.RatingKey, typ, title string, year int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO catalog_titles(key,type,tmdb_id,title,year) VALUES(?,?,?,?,?)`,
		int64(key), typ, key.TMDB(), title, year); err != nil {
		t.Fatal(err)
	}
}
