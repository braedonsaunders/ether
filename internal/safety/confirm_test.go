package safety

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestConfirmPlaybackSeesDurableRestrictionBeforeCacheReload(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := users.BootstrapOwner(ctx, config.Plex{OwnerPlexID: 7, OwnerUsername: "owner", Token: "server-token"}); err != nil {
		t.Fatal(err)
	}
	if err := users.SyncFriends(ctx, []plextv.Friend{{PlexID: 9, Username: "friend", Token: "friend-token", Sections: []string{"1"}}}); err != nil {
		t.Fatal(err)
	}
	friend, err := users.Authenticate(ctx, "friend-token")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	cached := *friend
	cached.AllowedSections = nil
	fresh, err := policy.ConfirmPlayback(ctx, &cached, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.AllowedSections) != 1 || fresh.AllowedSections[0] != 1 {
		t.Fatalf("fresh sections = %#v", fresh.AllowedSections)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=?`, friend.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Authenticate(ctx, "friend-token"); err != nil {
		t.Fatal("cache should still accept the token before reload")
	}
	if _, err := policy.ConfirmPlayback(ctx, friend, false, time.Now()); err != core.ErrUnauthorized {
		t.Fatalf("deactivated viewer: %v", err)
	}
}
