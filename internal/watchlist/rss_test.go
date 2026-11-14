package watchlist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type rssFakeTrakt struct {
	mu     sync.Mutex
	linked map[int64]bool
	pushed []string
}

func (f *rssFakeTrakt) Watchlist(context.Context, int64, int) ([]core.Title, error) {
	return nil, nil
}

func (f *rssFakeTrakt) SetWatchlist(_ context.Context, userID int64, title core.Title, add bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.linked[userID] {
		return false, nil
	}
	f.pushed = append(f.pushed, fmt.Sprintf("%d:%d:%t", userID, title.Key, add))
	return true, nil
}

func TestPlexRSSFeedImportLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	sealer, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sealer.UpgradeCredentials(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	movie1 := core.Title{
		Key: core.MovieKey(101), Type: core.TypeMovie, Title: "Dune", Year: 2021,
		ContentRating: "PG-13", Released: now.Add(-48 * time.Hour),
		Rating: 8.0, Popularity: 80, VoteCount: 1000, PosterPath: "/dune.jpg",
		IDs: core.IDs{TMDB: 101, IMDB: "tt1160419"},
	}
	movie2 := core.Title{
		Key: core.MovieKey(102), Type: core.TypeMovie, Title: "Arrival", Year: 2016,
		ContentRating: "PG-13", Released: now.Add(-48 * time.Hour),
		Rating: 7.9, Popularity: 75, VoteCount: 1000, PosterPath: "/arrival.jpg",
		IDs: core.IDs{TMDB: 102, IMDB: "tt2543164"},
	}
	show1 := core.Title{
		Key: core.ShowKey(201), Type: core.TypeShow, Title: "Severance", Year: 2022,
		ContentRating: "TV-MA", Released: now.Add(-48 * time.Hour),
		Rating: 8.7, Popularity: 90, VoteCount: 1000, PosterPath: "/severance.jpg",
		IDs: core.IDs{TMDB: 201, TVDB: 371980},
	}
	upcomingMovie := core.Title{
		Key: core.MovieKey(104), Type: core.TypeMovie, Title: "Dune Messiah", Year: 2026,
		ContentRating: "PG-13", Released: now.Add(24 * time.Hour),
		Rating: 8.2, Popularity: 85, VoteCount: 500, PosterPath: "/messiah.jpg",
		IDs: core.IDs{TMDB: 104, IMDB: "tt15239678"},
	}
	movieOwner := core.Title{
		Key: core.MovieKey(103), Type: core.TypeMovie, Title: "Blade Runner 2049", Year: 2017,
		ContentRating: "R", Released: now.Add(-48 * time.Hour),
		Rating: 8.0, Popularity: 70, VoteCount: 1000, PosterPath: "/br2049.jpg",
		IDs: core.IDs{TMDB: 103, IMDB: "tt1856101"},
	}

	catCfg := config.Catalog{Language: "en-US", Region: "US", MinPopularity: 1, MinVotes: 1}
	cat, err := catalog.New(db, nil, catCfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []core.Title{movie1, movie2, show1, upcomingMovie, movieOwner} {
		if err := cat.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}

	cols, err := collections.New(
		db,
		cat,
		slog.Default(),
		collections.WithIndexTTL(0),
		collections.WithLiveTTL(0),
		collections.WithNow(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}

	owner := core.User{ID: 1, PlexID: 42, Username: "owner", Title: "Owner", IsOwner: true}
	alice := core.User{ID: 2, PlexID: 1001, Username: "alice", Title: "Alice", Email: "alice@example.com"}
	teen := core.User{ID: 3, PlexID: 1002, Username: "teen", Title: "Teen", MaxContentRating: "PG-13"}
	moviesOnly := core.User{ID: 4, PlexID: 1003, Username: "moviesonly", Title: "Movies Only", AllowedLibraries: []core.LibraryID{core.LibraryMovies}}
	strictKid := core.User{ID: 5, PlexID: 1004, Username: "kid", Title: "Kid", MaxContentRating: "G"}
	users := []core.User{owner, alice, teen, moviesOnly, strictKid}

	var mu sync.Mutex
	etag := `"feed-v1"`
	lastMod := "Tue, 10 Nov 2026 15:00:00 GMT"
	// Real rss.plex.tv Friends' Watchlist XML carries no per-friend attribution.
	friendsXML := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Plex Watchlist for Owner's Friends</title>
    <item>
      <title>Dune (2021)</title>
      <category>movie</category>
      <guid isPermaLink="false">imdb://tt1160419</guid>
    </item>
    <item>
      <title>Arrival (2016)</title>
      <category>movie</category>
      <guid isPermaLink="false">tmdb://102</guid>
    </item>
    <item>
      <title>Severance (2022)</title>
      <category>show</category>
      <guid isPermaLink="false">tvdb://371980</guid>
    </item>
    <item>
      <title>Dune Messiah (2026)</title>
      <category>movie</category>
      <guid isPermaLink="false">imdb://tt15239678</guid>
    </item>
    <item>
      <title>Dune (2021)</title>
      <category>movie</category>
      <link>https://www.themoviedb.org/movie/101-dune</link>
    </item>
    <item>
      <title>Unknown Catalogue Title</title>
      <category>movie</category>
      <guid isPermaLink="false">imdb://tt9999999</guid>
    </item>
  </channel>
</rss>`
	ownerXML := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Your Watchlist</title>
    <item>
      <title>Blade Runner 2049 (2017)</title>
      <category>movie</category>
      <guid isPermaLink="false">imdb://tt1856101</guid>
    </item>
  </channel>
</rss>`

	var friendsHits, friends304 int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/friends-secret-uuid":
			friendsHits++
			if r.Header.Get("If-None-Match") == etag && r.Header.Get("If-Modified-Since") == lastMod {
				friends304++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			w.Header().Set("Last-Modified", lastMod)
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(friendsXML))
		case "/owner-secret-uuid":
			if r.Header.Get("If-None-Match") == `"owner-v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"owner-v1"`)
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(ownerXML))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	trakt := &rssFakeTrakt{linked: map[int64]bool{owner.ID: true, alice.ID: true}}
	res := &fakeResolver{}
	eng := &fakeEngine{}
	prov := &fakeProvenance{}
	note := &fakeNotifier{}
	var availMu sync.Mutex
	var onAvail []string
	s := New(db, Options{
		Sealer:   sealer,
		Catalog:  cat,
		Resolver: res,
		Engine:   eng,
		Cache:    prov,
		Notifier: note,
		Trakt:    trakt,
		Users:    func(context.Context) ([]core.User, error) { return users, nil },
		Now:      func() time.Time { return now },
		OnAvailable: func(_ context.Context, userID int64, key core.RatingKey) {
			availMu.Lock()
			onAvail = append(onAvail, fmt.Sprintf("%d:%d", userID, key))
			availMu.Unlock()
		},
		OnFriendsRSSChanged: func(ctx context.Context) {
			if err := cols.SyncFriendsWatchlist(ctx); err != nil {
				t.Errorf("SyncFriendsWatchlist: %v", err)
			}
		},
	})

	// Alice has movie1 on her own Plex watchlist: must stay isolated from the household Friends' RSS set.
	if err := s.upsert(ctx, alice.ID, "plex", movie1); err != nil {
		t.Fatal(err)
	}

	// Save settings via HTTP router and verify sealing at rest + redaction in API.
	friendsURL := srv.URL + "/friends-secret-uuid"
	ownerURL := srv.URL + "/owner-secret-uuid"
	putBody, _ := json.Marshal(Settings{
		Enabled:           true,
		Plex:              true,
		PlexFriendsRSSURL: friendsURL,
		PlexOwnerRSSURL:   ownerURL,
		Notify:            true,
		Prewarm:           true,
		CheckHours:        6,
	})
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/settings", bytes.NewReader(putBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /settings = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "friends-secret-uuid") || strings.Contains(rec.Body.String(), "owner-secret-uuid") {
		t.Fatalf("API response leaked RSS URL: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "skippedFriends") {
		t.Fatalf("API response still exposes dead skippedFriends field: %s", rec.Body.String())
	}
	var apiCfg Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &apiCfg); err != nil {
		t.Fatal(err)
	}
	if apiCfg.PlexFriendsRSSURL != Redacted || apiCfg.PlexOwnerRSSURL != Redacted {
		t.Fatalf("redaction mismatch: %+v", apiCfg)
	}
	var rawRow string
	if err := db.QueryRow(`SELECT value FROM watchlist_settings WHERE id=1`).Scan(&rawRow); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawRow, "friends-secret-uuid") || !strings.Contains(rawRow, "ether-sealed:v2:") {
		t.Fatalf("settings row not sealed: %s", rawRow)
	}

	// Saving back the redacted placeholder keeps the sealed URLs intact.
	putRedacted, _ := json.Marshal(apiCfg)
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, httptest.NewRequest(http.MethodPut, "/settings", bytes.NewReader(putRedacted)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("PUT redacted = %d: %s", rec2.Code, rec2.Body.String())
	}

	// Poll 1: Friends' Watchlist RSS populates one household-scoped set (user_id=0, source="plex-friends-rss");
	// Your Watchlist RSS still attributes to Owner and pushes to Owner's Trakt.
	if err := s.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	gotCfg, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fst := gotCfg.PlexFriendsRSSStatus
	if fst == nil || !fst.Configured || fst.LastRead != now.Unix() || fst.Imported != 4 || fst.UnmatchedTitles != 1 || fst.Problem != "" {
		t.Fatalf("friends status = %+v", fst)
	}
	ost := gotCfg.PlexOwnerRSSStatus
	if ost == nil || !ost.Configured || ost.Imported != 1 || ost.UnmatchedTitles != 0 || ost.Problem != "" {
		t.Fatalf("owner status = %+v", ost)
	}

	// Stored household rows hold all 4 resolved titles (including upcomingMovie);
	// HouseholdItems returns the 3 released ones in feed order, with no viewer attribution.
	var storedHousehold int
	if err := db.QueryRow(`SELECT COUNT(*) FROM watchlist_items WHERE user_id=0 AND source='plex-friends-rss'`).Scan(&storedHousehold); err != nil || storedHousehold != 4 {
		t.Fatalf("stored household rows = %d (%v), want 4", storedHousehold, err)
	}
	household, err := s.HouseholdItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotHouseholdKeys := make([]core.RatingKey, 0, len(household))
	for _, it := range household {
		if !slices.Equal(it.Sources, []string{"plex-friends-rss"}) {
			t.Fatalf("household item sources = %v, want [plex-friends-rss]", it.Sources)
		}
		gotHouseholdKeys = append(gotHouseholdKeys, it.Title.Key)
	}
	wantReleasedKeys := []core.RatingKey{movie1.Key, movie2.Key, show1.Key}
	if !slices.Equal(gotHouseholdKeys, wantReleasedKeys) {
		t.Fatalf("released household keys = %v, want %v", gotHouseholdKeys, wantReleasedKeys)
	}

	// Viewers' personal watchlists are NOT polluted by Friends' RSS; Owner still gets Your Watchlist RSS + Trakt push.
	if aliceItems, err := s.Items(ctx, &alice); err != nil || len(aliceItems) != 1 || aliceItems[0].Title.Key != movie1.Key || !slices.Equal(aliceItems[0].Sources, []string{"plex"}) {
		t.Fatalf("alice items = %+v (%v)", aliceItems, err)
	}
	if teenItems, err := s.Items(ctx, &teen); err != nil || len(teenItems) != 0 {
		t.Fatalf("teen items = %+v (%v)", teenItems, err)
	}
	if ownerItems, err := s.Items(ctx, &owner); err != nil || len(ownerItems) != 1 || ownerItems[0].Title.Key != movieOwner.Key {
		t.Fatalf("owner items = %+v (%v)", ownerItems, err)
	}
	trakt.mu.Lock()
	pushedCopy := append([]string(nil), trakt.pushed...)
	trakt.mu.Unlock()
	if !slices.Equal(pushedCopy, []string{fmt.Sprintf("%d:%d:true", owner.ID, movieOwner.Key)}) {
		t.Fatalf("trakt pushes = %v (only owner's RSS should push to Trakt)", pushedCopy)
	}

	// Collection & Home/section row "Your friends want to watch" appear via existing collections machinery,
	// with released titles in feed order, nothing naming any friend, and per-viewer content/library rules applied.
	findFriendsCol := func(u *core.User, media core.MediaType) *core.Collection {
		t.Helper()
		list, err := cols.List(ctx, u, media)
		if err != nil {
			t.Fatalf("cols.List(%s): %v", u.Username, err)
		}
		for i := range list {
			if list[i].Title == collections.FriendsWatchlistTitle {
				return &list[i]
			}
		}
		return nil
	}
	findFriendsRow := func(u *core.User) *core.CollectionRow {
		t.Helper()
		rows, err := cols.HomeRowsFor(ctx, u, core.HubContext{})
		if err != nil {
			t.Fatalf("cols.HomeRowsFor(%s): %v", u.Username, err)
		}
		for i := range rows {
			if rows[i].Hub.Title == collections.FriendsWatchlistTitle {
				return &rows[i]
			}
		}
		return nil
	}
	pageKeys := func(items []core.Title) []core.RatingKey {
		out := make([]core.RatingKey, 0, len(items))
		for _, it := range items {
			out = append(out, it.Key)
		}
		return out
	}

	aliceCol := findFriendsCol(&alice, "")
	if aliceCol == nil {
		t.Fatalf("expected %q collection for alice", collections.FriendsWatchlistTitle)
	}
	for _, u := range users {
		if strings.Contains(strings.ToLower(aliceCol.Title+" "+aliceCol.Summary), u.Username) {
			t.Fatalf("collection leaked viewer name %q: title=%q summary=%q", u.Username, aliceCol.Title, aliceCol.Summary)
		}
	}
	alicePage, err := cols.Items(ctx, &alice, aliceCol.Key, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pageKeys(alicePage.Items), []core.RatingKey{movie1.Key, movie2.Key, show1.Key}) {
		t.Fatalf("alice collection items = %v, want [movie1 movie2 show1]", pageKeys(alicePage.Items))
	}
	if movieSectionCol := findFriendsCol(&alice, core.TypeMovie); movieSectionCol == nil || movieSectionCol.ChildCount != 2 {
		t.Fatalf("movie section collection = %+v", movieSectionCol)
	}
	if showSectionCol := findFriendsCol(&alice, core.TypeShow); showSectionCol == nil || showSectionCol.ChildCount != 1 {
		t.Fatalf("show section collection = %+v", showSectionCol)
	}
	aliceRow := findFriendsRow(&alice)
	if aliceRow == nil || !aliceRow.HomeRow || !aliceRow.SectionRow || !aliceRow.Hub.Promoted {
		t.Fatalf("alice Home row = %+v", aliceRow)
	}
	if !slices.Equal(pageKeys(aliceRow.Hub.Items), []core.RatingKey{movie1.Key, movie2.Key, show1.Key}) {
		t.Fatalf("alice Home row items = %v, want [movie1 movie2 show1]", pageKeys(aliceRow.Hub.Items))
	}

	// Content rating & library restrictions apply per viewer.
	teenCol := findFriendsCol(&teen, "")
	if teenCol == nil {
		t.Fatal("expected collection for teen")
	}
	teenPage, err := cols.Items(ctx, &teen, teenCol.Key, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pageKeys(teenPage.Items), []core.RatingKey{movie1.Key, movie2.Key}) {
		t.Fatalf("teen (PG-13) collection items = %v, want TV-MA filtered out", pageKeys(teenPage.Items))
	}
	if teenRow := findFriendsRow(&teen); teenRow == nil || !slices.Equal(pageKeys(teenRow.Hub.Items), []core.RatingKey{movie1.Key, movie2.Key}) {
		t.Fatalf("teen (PG-13) Home row = %+v", teenRow)
	}
	if moviesRow := findFriendsRow(&moviesOnly); moviesRow == nil || !slices.Equal(pageKeys(moviesRow.Hub.Items), []core.RatingKey{movie1.Key, movie2.Key}) {
		t.Fatalf("moviesOnly Home row = %+v, want show filtered out", moviesRow)
	}
	if findFriendsCol(&strictKid, "") != nil || findFriendsRow(&strictKid) != nil {
		t.Fatal("strictKid (G) should not see collection or row when no items pass rating cap")
	}

	// Availability monitoring and prefetch work on the household set:
	// released movies movie1 & movie2 resolve, notify, warm cache with CacheCauseWatchlist, and fire OnAvailable;
	// upcomingMovie is held back until its release date arrives.
	if err := s.CheckHousehold(ctx); err != nil {
		t.Fatal(err)
	}
	if res.calls != 2 || eng.calls != 2 || note.calls != 2 || !prov.has("release-1", core.CacheCauseWatchlist) {
		t.Fatalf("household check before upcoming release: resolver=%d engine=%d notify=%d prov=%+v", res.calls, eng.calls, note.calls, prov.seen)
	}
	availMu.Lock()
	gotAvail1 := append([]string(nil), onAvail...)
	availMu.Unlock()
	if !slices.Equal(gotAvail1, []string{fmt.Sprintf("0:%d", movie1.Key), fmt.Sprintf("0:%d", movie2.Key)}) {
		t.Fatalf("OnAvailable after CheckHousehold = %v", gotAvail1)
	}

	// Poll 2: conditional request hits 304 Not Modified; items and counts remain intact, LastRead advances.
	now = now.Add(30 * time.Minute)
	if err := s.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotHits, got304 := friendsHits, friends304
	mu.Unlock()
	if gotHits != 2 || got304 != 1 {
		t.Fatalf("conditional request: hits=%d 304s=%d", gotHits, got304)
	}
	gotCfg, _ = s.Settings(ctx)
	if gotCfg.PlexFriendsRSSStatus.LastRead != now.Unix() || gotCfg.PlexFriendsRSSStatus.Imported != 4 || gotCfg.PlexFriendsRSSStatus.UnmatchedTitles != 1 {
		t.Fatalf("status after 304 = %+v", gotCfg.PlexFriendsRSSStatus)
	}

	// Advance past upcomingMovie's release date (48h > CheckHours): CheckHousehold re-verifies movie1 & movie2
	// without re-notifying or re-warming them, and newly notifies + prefetches upcomingMovie!
	now = now.Add(48 * time.Hour)
	if err := s.CheckHousehold(ctx); err != nil {
		t.Fatal(err)
	}
	if res.calls != 5 || eng.calls != 3 || note.calls != 3 {
		t.Fatalf("upcomingMovie release monitoring: resolver=%d engine=%d notify=%d", res.calls, eng.calls, note.calls)
	}
	availMu.Lock()
	gotAvail2 := append([]string(nil), onAvail...)
	availMu.Unlock()
	if !slices.Equal(gotAvail2, []string{fmt.Sprintf("0:%d", movie1.Key), fmt.Sprintf("0:%d", movie2.Key), fmt.Sprintf("0:%d", upcomingMovie.Key)}) {
		t.Fatalf("OnAvailable after upcoming release = %v", gotAvail2)
	}

	// Poll 3: feed updates (new ETag), removing Arrival, Severance, and Dune Messiah — leaving only Dune.
	// Removal updates the household set AND the collection/Home row, while keeping Alice's personal watchlist intact.
	mu.Lock()
	etag = `"feed-v2"`
	friendsXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <item>
      <title>Dune (2021)</title>
      <category>movie</category>
      <guid isPermaLink="false">imdb://tt1160419</guid>
    </item>
  </channel>
</rss>`
	mu.Unlock()

	now = now.Add(30 * time.Minute)
	if err := s.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	gotCfg, _ = s.Settings(ctx)
	if gotCfg.PlexFriendsRSSStatus.Imported != 1 || gotCfg.PlexFriendsRSSStatus.UnmatchedTitles != 0 {
		t.Fatalf("status after feed removal = %+v", gotCfg.PlexFriendsRSSStatus)
	}
	householdAfter, err := s.HouseholdItems(ctx)
	if err != nil || len(householdAfter) != 1 || householdAfter[0].Title.Key != movie1.Key {
		t.Fatalf("household after removal = %+v (%v)", householdAfter, err)
	}
	if aliceRowAfter := findFriendsRow(&alice); aliceRowAfter == nil || !slices.Equal(pageKeys(aliceRowAfter.Hub.Items), []core.RatingKey{movie1.Key}) {
		t.Fatalf("alice row after removal = %+v", aliceRowAfter)
	}
	if aliceAfter, err := s.Items(ctx, &alice); err != nil || len(aliceAfter) != 1 || aliceAfter[0].Title.Key != movie1.Key {
		t.Fatalf("alice personal watchlist after feed removal = %+v (%v)", aliceAfter, err)
	}

	// Clearing PlexFriendsRSSURL empties the household set and hides the shared row/collection.
	if err := s.SaveSettings(ctx, Settings{Enabled: true, Plex: true, PlexFriendsRSSURL: "", PlexOwnerRSSURL: Redacted, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	if householdCleared, err := s.HouseholdItems(ctx); err != nil || len(householdCleared) != 0 {
		t.Fatalf("household after URL clear = %+v (%v)", householdCleared, err)
	}
	if findFriendsCol(&alice, "") != nil || findFriendsRow(&alice) != nil {
		t.Fatal("expected collection and Home row to hide once Friends' RSS set is empty")
	}
}

func TestPlexRSSFollowsRedirect(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	sealer, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sealer.UpgradeCredentials(ctx); err != nil {
		t.Fatal(err)
	}

	cat, err := catalog.New(db, nil, config.Catalog{Language: "en-US", Region: "US", MinPopularity: 1, MinVotes: 1}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	movie := core.Title{
		Key:        core.MovieKey(999),
		Type:       core.TypeMovie,
		Title:      "Redirected Film",
		Year:       2024,
		Released:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Rating:     8.0,
		Popularity: 80,
		VoteCount:  1000,
		PosterPath: "/film.jpg",
		IDs:        core.IDs{TMDB: 999, IMDB: "tt9991111"},
	}
	if err := cat.UpsertTitle(ctx, movie, true); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/friends":
			http.Redirect(w, r, "/target", http.StatusFound)
		case "/target":
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Friends</title>
<item><title>Redirected Film (2024)</title><guid>imdb://tt9991111</guid><category>movie</category></item>
</channel></rss>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Do not pass Client so New() configures the default CheckRedirect = ErrUseLastResponse.
	s := New(db, Options{
		Catalog: cat,
		Sealer:  sealer,
	})
	if err := s.SaveSettings(ctx, Settings{
		Enabled:           true,
		Plex:              true,
		PlexFriendsRSSURL: srv.URL + "/friends",
		CheckHours:        6,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncRSS(ctx); err != nil {
		t.Fatalf("SyncRSS failed on 302 redirect: %v", err)
	}
	items, err := s.HouseholdItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title.Key != movie.Key {
		t.Fatalf("expected redirected RSS item %d, got %+v", movie.Key, items)
	}
	// Ensure the shared Client still refuses redirects for Community/Plex token requests.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/friends", nil)
	resp, err := s.o.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected shared s.o.Client to keep 302, got %d", resp.StatusCode)
	}
}

func TestPlexFriendsRSSAttributesAuthorToViewer(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	sealer, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sealer.UpgradeCredentials(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 14, 13, 0, 0, 0, time.UTC)
	show := core.Title{
		Key: core.ShowKey(201), Type: core.TypeShow, Title: "The Rings of Power", Year: 2022,
		ContentRating: "TV-14", Released: now.Add(-48 * time.Hour),
		Rating: 7.0, Popularity: 80, VoteCount: 1000, PosterPath: "/rop.jpg",
		IDs: core.IDs{TMDB: 201, TVDB: 367506},
	}

	catCfg := config.Catalog{Language: "en-US", Region: "US", MinPopularity: 1, MinVotes: 1}
	cat, err := catalog.New(db, nil, catCfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertTitle(ctx, show, true); err != nil {
		t.Fatal(err)
	}

	viewer := core.User{ID: 20, PlexID: 143405302, Username: "Aayoshi", Title: "Aayoshi"}
	rssServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Plex Watchlist</title>
    <item>
      <author>93f1c58b6f64a6dc</author>
      <category>show</category>
      <title>The Lord of the Rings: The Rings of Power (2022)</title>
      <guid isPermaLink="false">tvdb://367506</guid>
    </item>
  </channel>
</rss>`))
	}))
	defer rssServer.Close()

	s := New(db, Options{
		Sealer:  sealer,
		Catalog: cat,
		Users:   func(context.Context) ([]core.User, error) { return []core.User{viewer}, nil },
		Now:     func() time.Time { return now },
		Logger:  slog.Default(),
	})

	s.recordFriend(ctx, FriendFeedStatus{ID: "93f1c58b6f64a6dc", Username: "Aayoshi"})
	if err := s.SaveSettings(ctx, Settings{Enabled: true, Plex: true, PlexFriendsRSSURL: rssServer.URL, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncRSS(ctx); err != nil {
		t.Fatalf("SyncRSS: %v", err)
	}

	viewerItems, err := s.Items(ctx, &viewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(viewerItems) != 1 || viewerItems[0].Title.Key != show.Key || !slices.Equal(viewerItems[0].Sources, []string{"plex-community:93f1c58b6f64a6dc"}) {
		t.Fatalf("expected attributed viewer item %d from plex-community:93f1c58b6f64a6dc, got %+v", show.Key, viewerItems)
	}
	if queued := s.drainRSSChecks(); len(queued) != 1 || queued[0].ID != viewer.ID {
		t.Fatalf("expected viewer %d queued for immediate availability check, got %+v", viewer.ID, queued)
	}
	if err := s.SyncRSS(ctx); err != nil {
		t.Fatalf("second SyncRSS: %v", err)
	}
	if queued := s.drainRSSChecks(); len(queued) != 0 {
		t.Fatalf("expected no duplicate check queued for existing item, got %+v", queued)
	}

	householdItems, err := s.Items(ctx, &core.User{ID: householdUserID})
	if err != nil {
		t.Fatal(err)
	}
	if len(householdItems) != 1 || householdItems[0].Title.Key != show.Key || !slices.Equal(householdItems[0].Sources, []string{sourcePlexFriendsRSS}) {
		t.Fatalf("expected household item %d from %s, got %+v", show.Key, sourcePlexFriendsRSS, householdItems)
	}
}
