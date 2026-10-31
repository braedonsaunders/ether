package catalog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// ratingTMDB serves movies whose certifications mirror the production
// catalogue: a Hallmark TV movie TMDB certifies "G" on its US TV premiere
// (type 6), one only Canada, Brazil and Germany rate, one nobody rates, and
// a show rated TV-PG. release lets a test change a movie's certifications
// for the light /release_dates endpoint.
func ratingTMDB(t *testing.T, release map[int64]*atomic.Value) *httptest.Server {
	t.Helper()
	movie := func(id int64, title, dates string) string {
		return fmt.Sprintf(`{"id":%d,"title":%q,"release_date":"2019-11-17","runtime":84,"status":"Released","vote_average":7,"vote_count":400,"popularity":20,"original_language":"en","genres":[{"id":10770,"name":"TV Movie"}],"release_dates":%s,"credits":{"cast":[],"crew":[]}}`, id, title, dates)
	}
	dates := map[int64]string{
		700: `{"results":[{"iso_3166_1":"BR","release_dates":[{"certification":"L","type":6}]},{"iso_3166_1":"DE","release_dates":[{"certification":"0","type":6}]},{"iso_3166_1":"US","release_dates":[{"certification":"G","type":6,"note":"Hallmark Channel"}]}]}`,
		701: `{"results":[{"iso_3166_1":"BR","release_dates":[{"certification":"L","type":6}]},{"iso_3166_1":"CA","release_dates":[{"certification":"G","type":6}]},{"iso_3166_1":"DE","release_dates":[{"certification":"6","type":6}]},{"iso_3166_1":"US","release_dates":[{"certification":"","type":6}]}]}`,
		702: `{"results":[{"iso_3166_1":"US","release_dates":[{"certification":"","type":6}]}]}`,
	}
	titles := map[int64]string{700: "Christmas in Evergreen", 701: "Write Before Christmas", 702: "Once Upon A Holiday"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		var id int64
		switch {
		case p == "/genre/movie/list", p == "/genre/tv/list":
			fmt.Fprint(w, `{"genres":[{"id":10770,"name":"TV Movie"},{"id":18,"name":"Drama"}]}`)
		case p == "/tv/61865":
			fmt.Fprint(w, `{"id":61865,"name":"When Calls the Heart","first_air_date":"2014-01-11","status":"Returning Series","vote_average":8,"vote_count":400,"popularity":30,"original_language":"en","seasons":[],"content_ratings":{"results":[{"iso_3166_1":"DE","rating":"12"},{"iso_3166_1":"US","rating":"TV-G"}]}}`)
		case p == "/tv/61865/content_ratings":
			fmt.Fprint(w, `{"results":[{"iso_3166_1":"US","rating":"TV-PG"}]}`)
		case strings.HasSuffix(p, "/release_dates"):
			fmt.Sscanf(p, "/movie/%d/release_dates", &id)
			if v, ok := release[id]; ok && v.Load() != nil {
				fmt.Fprint(w, v.Load())
				return
			}
			fmt.Fprint(w, dates[id])
		default:
			if _, err := fmt.Sscanf(p, "/movie/%d", &id); err == nil && titles[id] != "" {
				fmt.Fprint(w, movie(id, titles[id], dates[id]))
				return
			}
			w.WriteHeader(404)
			fmt.Fprint(w, `{"status_message":"not found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ratingCatalog(t *testing.T, srv *httptest.Server, cfg config.Catalog) (*catalog.Catalog, *sql.DB) {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	client := tmdb.New("testkey", slog.Default(), tmdb.WithBaseURL(srv.URL), tmdb.WithCacheTTL(0))
	t.Cleanup(client.Close)
	cfg.Language, cfg.MinPopularity, cfg.MinVotes, cfg.RefreshInterval = "en-US", 1, 1, config.Duration(24*time.Hour)
	c, err := catalog.New(db, client, cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return c, db
}

func stored(t *testing.T, db *sql.DB, key core.RatingKey) (value string, rank int, source string) {
	t.Helper()
	if err := db.QueryRow(`SELECT content_rating,content_rating_rank,content_rating_source FROM catalog_titles WHERE key=?`, int64(key)).Scan(&value, &rank, &source); err != nil {
		t.Fatal(err)
	}
	return value, rank, source
}

func TestCertificationSelectionAndLibraryAllowlist(t *testing.T) {
	ctx := context.Background()
	srv := ratingTMDB(t, nil)
	allow := json.RawMessage(`["G","PG","TV-G","TV-PG"]`)
	c, db := ratingCatalog(t, srv, config.Catalog{Region: "US", Exclusions: []core.LibraryExclusion{{Name: "Family", MediaType: "mixed", Match: "all",
		Rules: []core.CollectionRule{{Field: "contentRating", Op: "notIn", Value: allow}}}}})
	for _, tc := range []struct {
		key           core.RatingKey
		value, source string
		rank          int
	}{
		{core.MovieKey(700), "G", "tmdb:US:6", 1},    // the Hallmark TV movie keeps its US label
		{core.MovieKey(701), "ca/G", "tmdb:CA:6", 1}, // foreign only: qualified, never passed off as US
		{core.MovieKey(702), "", "", 0},              // nobody rates it
		{core.ShowKey(61865), "TV-G", "tmdb:US", 1},
	} {
		if _, err := c.StoredTitle(ctx, tc.key); err == nil {
			t.Fatalf("%v already stored", tc.key)
		}
		if _, err := c.Get(ctx, tc.key); err != nil {
			t.Fatalf("Get %v: %v", tc.key, err)
		}
		value, rank, source := stored(t, db, tc.key)
		if value != tc.value || rank != tc.rank || source != tc.source {
			t.Errorf("%v stored %q rank %d source %q, want %q %d %q", tc.key, value, rank, source, tc.value, tc.rank, tc.source)
		}
	}
	// G, PG, TV-G and TV-PG keep the Hallmark movie, the Canada-only movie
	// and the TV-G series; the unrated movie is hidden until "Not rated" is
	// listed.
	page, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[core.RatingKey]bool{}
	for _, it := range page.Items {
		seen[it.Key] = true
	}
	if !seen[core.MovieKey(700)] || !seen[core.MovieKey(701)] || !seen[core.ShowKey(61865)] || seen[core.MovieKey(702)] {
		t.Fatalf("allowlist browse = %v", seen)
	}
	for _, key := range []core.RatingKey{core.MovieKey(700), core.MovieKey(701), core.ShowKey(61865)} {
		st, _ := c.StoredTitle(ctx, key)
		if !c.Visible(st) {
			t.Errorf("%v hidden by the in-memory filter", key)
		}
	}
	// A restricted viewer capped at TV-PG sees both levels by SQL too.
	capped, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{MaxContentRating: "TV-PG"}, Limit: 50})
	if err != nil || len(capped.Items) != 3 {
		t.Fatalf("capped browse %d %v", len(capped.Items), err)
	}
}

func TestProviderCertificationPrecedence(t *testing.T) {
	ctx := context.Background()
	c, db := ratingCatalog(t, ratingTMDB(t, nil), config.Catalog{Region: "US"})
	for _, id := range []int64{700, 701, 702} {
		_, _ = c.Get(ctx, core.MovieKey(id))
	}
	// OMDb (IMDb's US certificate) fills the unrated title and replaces the
	// Canada-only rating, as Plex Media Server does, but never TMDB's US one.
	for id, label := range map[int64]string{700: "TV-PG", 701: "TV-G", 702: "TV-G"} {
		if err := c.ApplyProviderCertification(ctx, core.MovieKey(id), label, "omdb"); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[int64][3]string{700: {"G", "tmdb:US:6"}, 701: {"TV-G", "omdb"}, 702: {"TV-G", "omdb"}} {
		if value, _, source := stored(t, db, core.MovieKey(id)); value != want[0] || source != want[1] {
			t.Errorf("movie %d = %q from %q, want %q from %q", id, value, source, want[0], want[1])
		}
	}
	// An unusable label changes nothing.
	if err := c.ApplyProviderCertification(ctx, core.MovieKey(702), "N/A", "omdb"); err != nil {
		t.Fatal(err)
	}
	if value, _, _ := stored(t, db, core.MovieKey(702)); value != "TV-G" {
		t.Fatalf("N/A replaced the provider rating: %q", value)
	}
	// A detail refresh where TMDB still has no US rating keeps the
	// provider's; the foreign-only title keeps it over Canada's.
	if _, err := db.Exec(`UPDATE catalog_titles SET detail_at=1 WHERE key IN (?,?)`, int64(core.MovieKey(701)), int64(core.MovieKey(702))); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{701, 702} {
		_, _ = c.Get(ctx, core.MovieKey(id))
		if value, _, source := stored(t, db, core.MovieKey(id)); value != "TV-G" || source != "omdb" {
			t.Errorf("refresh of %d replaced the provider rating: %q %q", id, value, source)
		}
	}
	unrated, err := c.UnratedTitles(ctx, 0, 10)
	if err != nil || len(unrated) != 0 {
		t.Fatalf("unrated after fill: %v %v", unrated, err)
	}
}

// Ratings stored by an older release are re-ranked at startup and
// re-selected from TMDB in the background: a stale foreign fallback becomes
// the qualified rating, and a later US certification replaces it.
func TestRefreshRatingsReselectsStoredTitles(t *testing.T) {
	ctx := context.Background()
	release := map[int64]*atomic.Value{701: {}}
	srv := ratingTMDB(t, release)
	c, db := ratingCatalog(t, srv, config.Catalog{Region: "US"})
	for _, key := range []core.RatingKey{core.MovieKey(700), core.MovieKey(701), core.ShowKey(61865)} {
		_, _ = c.Get(ctx, key)
	}
	// What the old selection stored: Brazil's label passed off as US, with
	// the old rank scale.
	if _, err := db.Exec(`UPDATE catalog_titles SET content_rating='L',content_rating_rank=9,content_rating_source='' WHERE key=?`, int64(core.MovieKey(701))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM catalog_meta WHERE k LIKE 'content_rating_%'`); err != nil {
		t.Fatal(err)
	}
	c2, err := catalog.New(db, tmdb.New("testkey", slog.Default(), tmdb.WithBaseURL(srv.URL), tmdb.WithCacheTTL(0)), config.Catalog{Region: "US", RefreshInterval: config.Duration(24 * time.Hour)}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	n, err := c2.RefreshRatings(ctx, 1)
	if err != nil || n != 1 {
		t.Fatalf("first pass %d %v", n, err)
	}
	// Every row was re-ranked on the new scale before the sweep reached it.
	if value, rank, _ := stored(t, db, core.MovieKey(701)); value != "L" || rank != 1 {
		t.Fatalf("re-rank: %q rank %d", value, rank)
	}
	release[701].Store(`{"results":[{"iso_3166_1":"US","release_dates":[{"certification":"TV-PG","type":6}]}]}`)
	if n, err = c2.RefreshRatings(ctx, 1); err != nil || n != 1 {
		t.Fatalf("second pass %d %v", n, err)
	}
	if value, rank, source := stored(t, db, core.MovieKey(701)); value != "TV-PG" || rank != 2 || source != "tmdb:US:6" {
		t.Fatalf("re-selected %q rank %d %q", value, rank, source)
	}
	if n, err = c2.RefreshRatings(ctx, 2); err != nil || n != 1 {
		t.Fatalf("third pass %d %v", n, err)
	}
	// The show's light endpoint now says TV-PG; its seasons follow.
	if value, _, _ := stored(t, db, core.ShowKey(61865)); value != "TV-PG" {
		t.Fatalf("show re-selected %q", value)
	}
	if n, err = c2.RefreshRatings(ctx, 2); err != nil || n != 0 {
		t.Fatalf("finishing pass %d %v", n, err)
	}
	release[701].Store(`{"results":[]}`)
	if n, err = c2.RefreshRatings(ctx, 2); err != nil || n != 0 {
		t.Fatalf("a finished model must not sweep again: %d %v", n, err)
	}
	if value, _, _ := stored(t, db, core.MovieKey(701)); value != "TV-PG" {
		t.Fatalf("finished sweep rewrote %q", value)
	}
}
