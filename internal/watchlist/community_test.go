package watchlist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestCommunityFriendsPagedSnapshots(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	us := []core.User{{ID: 1, PlexID: 77, Username: "viewer"}}
	calls := 0
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "owner" {
			t.Error("owner token missing")
		}
		var body struct {
			Query     string `json:"query"`
			Variables struct {
				User struct {
					ID string `json:"id"`
				} `json:"user"`
				After *string `json:"after"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Variables.User.ID == "" {
			fmt.Fprint(w, `{"data":{"allFriendsV2":[{"user":{"id":"77","username":"viewer"}},{"user":{"id":"uuid-other","username":"other"}}]}}`)
			return
		}
		calls++
		if fail {
			fmt.Fprint(w, `{"errors":[{"message":"private"}]}`)
			return
		}
		id := "tmdb://101"
		next := true
		cursor := "second"
		if body.Variables.After != nil {
			id = "tmdb://102"
			next = false
			cursor = ""
		}
		fmt.Fprintf(w, `{"data":{"userV2":{"watchlist":{"nodes":[{"id":%q,"type":"movie"}],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}}}}`, id, next, cursor)
	}))
	defer server.Close()
	titles := map[core.RatingKey]core.Title{}
	for _, id := range []int64{101, 102} {
		titles[core.MovieKey(id)] = core.Title{Key: core.MovieKey(id), Type: core.TypeMovie, Title: "Movie"}
	}
	s := New(db, Options{Users: func(context.Context) ([]core.User, error) { return us, nil }, Catalog: fakeCatalog{titles: titles}, Community: &plextv.CommunityClient{Client: server.Client(), URL: server.URL}, OwnerToken: func(context.Context) (string, error) { return "owner", nil }, Tokens: func(context.Context, int64) (string, error) { return "", core.ErrUnauthorized }})
	if err := s.SaveSettings(ctx, Settings{Enabled: true, Plex: true, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	if err := s.syncCommunityFriends(ctx, us, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("watchlist pages=%d", calls)
	}
	for _, uid := range []int64{0, 1} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM watchlist_items WHERE user_id=?`, uid).Scan(&count); err != nil || count != 2 {
			t.Fatalf("user %d: count=%d err=%v", uid, count, err)
		}
	}
	// A later failed page must not erase either successful snapshot.
	fail = true
	if err := s.Sync(ctx, &us[0]); err == nil {
		t.Fatal("expected upstream failure")
	}
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM watchlist_items WHERE user_id=1`).Scan(&count)
	if count != 2 {
		t.Fatalf("lost snapshot: %d", count)
	}
	statuses := s.friendStatuses(ctx)
	if len(statuses) != 2 || statuses[0].Error == "" {
		t.Fatalf("missing per-friend status: %+v", statuses)
	}
}

func TestCommunityTitlesUppercaseType(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"userV2":{"watchlist":{"nodes":[{"id":"5d77682a880197001ec96873","type":"MOVIE"},{"id":"tmdb://202","type":"SHOW"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`)
	}))
	defer server.Close()

	movieKey := core.MovieKey(101)
	showKey := core.ShowKey(202)
	titles := map[core.RatingKey]core.Title{
		movieKey: {Key: movieKey, Type: core.TypeMovie, Title: "Movie"},
		showKey:  {Key: showKey, Type: core.TypeShow, Title: "Show"},
	}
	var lookedUp []string
	s := New(db, Options{
		Catalog:   fakeCatalog{titles: titles},
		Community: &plextv.CommunityClient{Client: server.Client(), URL: server.URL},
		PlexLookup: func(_ context.Context, guid string) (core.RatingKey, error) {
			lookedUp = append(lookedUp, guid)
			if guid == "plex://movie/5d77682a880197001ec96873" {
				return movieKey, nil
			}
			return 0, core.ErrNotFound
		},
	})
	got, unmatched, err := s.communityTitles(ctx, "owner", "friend-1")
	if err != nil {
		t.Fatal(err)
	}
	if unmatched != 0 || len(got) != 2 {
		t.Fatalf("got=%d unmatched=%d (%+v)", len(got), unmatched, got)
	}
	if len(lookedUp) != 1 || lookedUp[0] != "plex://movie/5d77682a880197001ec96873" {
		t.Fatalf("lookedUp=%v", lookedUp)
	}
}

func TestCommunityTitlesSkipsAmbiguousIdentityAndPreservesRSS(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"userV2":{"watchlist":{"nodes":[{"id":"ambiguous-guid","type":"MOVIE"},{"id":"valid-guid","type":"MOVIE"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`)
	}))
	defer server.Close()

	movieKey := core.MovieKey(101)
	rssKey := core.MovieKey(202)
	movieTitle := core.Title{Key: movieKey, Type: core.TypeMovie, Title: "Valid Movie"}
	rssTitle := core.Title{Key: rssKey, Type: core.TypeMovie, Title: "RSS Only Movie"}
	s := New(db, Options{
		Catalog:   fakeCatalog{titles: map[core.RatingKey]core.Title{movieKey: movieTitle}},
		Community: &plextv.CommunityClient{Client: server.Client(), URL: server.URL},
		PlexLookup: func(_ context.Context, guid string) (core.RatingKey, error) {
			if guid == "plex://movie/ambiguous-guid" {
				return 0, errors.New("ambiguous Plex identity")
			}
			if guid == "plex://movie/valid-guid" {
				return movieKey, nil
			}
			return 0, core.ErrNotFound
		},
	})

	got, unmatched, err := s.communityTitles(ctx, "owner", "friend-1")
	if err != nil {
		t.Fatalf("communityTitles error: %v", err)
	}
	if unmatched != 1 || len(got) != 1 || got[0].Key != movieKey {
		t.Fatalf("got=%+v unmatched=%d, want 1 valid and 1 unmatched", got, unmatched)
	}

	const source = "plex-community:friend-1"
	const viewerID int64 = 20
	if err := s.upsert(ctx, viewerID, source, rssTitle); err != nil {
		t.Fatal(err)
	}
	if err := s.upsert(ctx, householdUserID, sourcePlexFriendsRSS, rssTitle); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO watchlist_rss_authors(friend_id,item_key) VALUES(?,?)`, "friend-1", rssKey); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	merged, err := s.preserveActiveFriendsRSSItems(ctx, tx, viewerID, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 || merged[0].Key != rssKey {
		t.Fatalf("preserveActiveFriendsRSSItems=%+v, want %d", merged, rssKey)
	}
}

// Snapshot replacement must preserve only this friend's current RSS proof,
// retain monitoring history through legacy upgrades, and roll back on failure.
func TestCommunitySnapshotAttributionAndRollback(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	u := &core.User{ID: 20}
	const source = "plex-community:friend-1"
	title := core.Title{Key: core.MovieKey(202), Type: core.TypeMovie, Title: "RSS"}
	if err := s.upsert(ctx, u.ID, "plex-community", title); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE watchlist_items SET checked=11,available=12,notified=1,warmed=1 WHERE user_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.replaceHouseholdFeedSnapshotWithAuthors(ctx, sourcePlexFriendsRSS, []core.Title{title}, map[string][]core.Title{"friend-1": {title}}); err != nil {
		t.Fatal(err)
	}
	// Malformed preserved metadata must abort both upgrade and replacement.
	if _, err := db.ExecContext(ctx, `UPDATE watchlist_items SET title='{' WHERE user_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.replaceOwnerFeedSnapshot(ctx, source, u, nil); err == nil {
		t.Fatal("corrupt metadata accepted")
	}
	var legacy int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM watchlist_items WHERE user_id=? AND source='plex-community'`, u.ID).Scan(&legacy); err != nil || legacy != 1 {
		t.Fatalf("upgrade did not roll back: %d %v", legacy, err)
	}
	if err := s.upsert(ctx, u.ID, "plex-community", title); err != nil {
		t.Fatal(err)
	}
	if _, err := s.replaceOwnerFeedSnapshot(ctx, source, u, nil); err != nil {
		t.Fatal(err)
	}
	var checked, available, notified, warmed int
	if err := db.QueryRowContext(ctx, `SELECT checked,available,notified,warmed FROM watchlist_items WHERE user_id=? AND source=?`, u.ID, source).Scan(&checked, &available, &notified, &warmed); err != nil {
		t.Fatal(err)
	}
	if checked != 11 || available != 12 || notified != 1 || warmed != 1 {
		t.Fatalf("monitoring history lost: %d %d %d %d", checked, available, notified, warmed)
	}
	// The same movie remaining in somebody else's RSS cannot pin this row.
	if _, err := s.replaceHouseholdFeedSnapshotWithAuthors(ctx, sourcePlexFriendsRSS, []core.Title{title}, map[string][]core.Title{"friend-2": {title}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.replaceOwnerFeedSnapshot(ctx, source, u, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM watchlist_items WHERE user_id=?`, u.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("another friend's RSS preserved viewer row: %d %v", count, err)
	}
}
