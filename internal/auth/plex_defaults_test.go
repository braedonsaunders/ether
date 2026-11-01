package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/braedonsaunders/ether/internal/plextv"
)

func userByName(t *testing.T, s *Service, name string) int64 {
	t.Helper()
	users, err := s.Users(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Username == name || u.Title == name {
			return u.ID
		}
	}
	t.Fatalf("no user %q", name)
	return 0
}

func TestPlexDefaultsCachedFromSync(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	off := false
	if err := s.SetOwner(ctx, plextv.Account{
		ID: 42, Username: "owner", Title: "The Owner",
		Track: plextv.TrackDefaults{AudioLanguage: "en", AutoSelectAudio: true, SubtitleLanguage: "fr", SubtitleMode: 2, HasProfile: true},
	}, "server-token"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFriends(ctx, []plextv.Friend{
		{Owned: true, Token: "owner-access", PlexID: 42},
		{PlexID: 100, Username: "alice", Title: "Alice", Token: "alice-access",
			Track: plextv.TrackDefaults{AudioLanguage: "ja", AutoSelectAudio: true, SubtitleLanguage: "en", SubtitleMode: 1, HasProfile: true},
			Share: plextv.ShareDefaults{FilterMovies: "contentRating<=PG-13", FilterTelevision: "label!=Hidden", AllowSync: &off, HasSettings: true}},
		{PlexID: 101, Username: "bob", Token: "bob-access", AllLibs: true},
	}); err != nil {
		t.Fatal(err)
	}

	ownerID := userByName(t, s, "owner")
	aliceID := userByName(t, s, "alice")
	owner, ok := s.PlexDefaults(ownerID)
	if !ok || owner.AudioLanguage != "en" || owner.EtherMode() != "always" {
		t.Fatalf("owner %+v ok=%v", owner, ok)
	}
	alice, ok := s.PlexDefaults(aliceID)
	if !ok || alice.AudioLanguage != "ja" || alice.FilterMovies != "contentRating<=PG-13" || alice.AllowSync == nil || *alice.AllowSync {
		t.Fatalf("alice %+v ok=%v", alice, ok)
	}

	req := httptest.NewRequest(http.MethodGet, "/"+strconv.FormatInt(aliceID, 10), nil)
	rec := httptest.NewRecorder()
	s.PlexDefaultsHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["subtitleMode"] != "foreign-only" || body["filterMovies"] != "contentRating<=PG-13" || body["allowSync"] != false {
		t.Fatalf("%s", rec.Body.String())
	}
}

func TestPlexDefaultsOwnerTrackSurvivesFriendSync(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if err := s.SetOwner(ctx, plextv.Account{
		ID: 42, Username: "owner", Title: "The Owner",
		Track: plextv.TrackDefaults{AudioLanguage: "en", AutoSelectAudio: true, HasProfile: true},
	}, "server-token"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFriends(ctx, []plextv.Friend{
		{Owned: true, Token: "owner-access", PlexID: 42},
	}); err != nil {
		t.Fatal(err)
	}
	d, ok := s.PlexDefaults(userByName(t, s, "owner"))
	if !ok || d.AudioLanguage != "en" {
		t.Fatalf("owner track lost: %+v ok=%v", d, ok)
	}
}

func TestPlexDefaultsRemovedWithFriend(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if err := s.SyncFriends(ctx, []plextv.Friend{
		{Owned: true, Token: "owner-access", PlexID: 42},
		{PlexID: 100, Username: "alice", Title: "Alice", Token: "alice-access",
			Track: plextv.TrackDefaults{AudioLanguage: "ja", AutoSelectAudio: true, HasProfile: true}},
	}); err != nil {
		t.Fatal(err)
	}
	aliceID := userByName(t, s, "alice")
	if _, ok := s.PlexDefaults(aliceID); !ok {
		t.Fatal("alice defaults missing")
	}
	if err := s.SyncFriends(ctx, []plextv.Friend{
		{Owned: true, Token: "owner-access", PlexID: 42},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.PlexDefaults(aliceID); ok {
		t.Fatal("removed friend kept defaults")
	}
}
