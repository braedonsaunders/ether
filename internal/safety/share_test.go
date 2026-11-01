package safety

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/store"
)

func shareService(t *testing.T, share PlexShare) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPlexShare(func(*core.User) PlexShare { return share })
	s.SetFriendsDownloads(func() bool { return true })
	return s
}

func TestPlexSharePrunesByType(t *testing.T) {
	off := false
	s := shareService(t, PlexShare{
		FilterMovies:     "contentRating<=PG-13",
		FilterTelevision: "contentRating<=TV-PG|label!=Hidden",
		AllowSync:        &off,
	})
	u := &core.User{ID: 3}
	pg := &core.Title{Type: core.TypeMovie, ContentRating: "PG"}
	r := &core.Title{Type: core.TypeMovie, ContentRating: "R"}
	tvok := &core.Title{Type: core.TypeShow, ContentRating: "TV-PG"}
	tvno := &core.Title{Type: core.TypeShow, ContentRating: "TV-MA"}
	hidden := &core.Title{Type: core.TypeShow, ContentRating: "TV-PG", CollectionName: "Hidden"}
	if !s.Allowed(u, pg) || s.Allowed(u, r) || !s.Allowed(u, tvok) || s.Allowed(u, tvno) || s.Allowed(u, hidden) {
		t.Fatal("share filters")
	}
	if !s.Restricted(u) {
		t.Fatal("restricted")
	}
	if !experience.ViewerRestricted(s, u) {
		t.Fatal("visibility restricted")
	}
	if experience.TitleAllowed(s, u, r) || !experience.TitleAllowed(s, u, pg) {
		t.Fatal("browse/search/hubs prune")
	}
}

func TestPlexShareOwnerRatingWins(t *testing.T) {
	s := shareService(t, PlexShare{FilterMovies: "contentRating<=G|label=Kids"})
	u := &core.User{ID: 3, MaxContentRating: "PG-13"}
	if !s.Allowed(u, &core.Title{Type: core.TypeMovie, ContentRating: "PG", CollectionName: "Kids"}) {
		t.Fatal("owner cap + matching label")
	}
	if s.Allowed(u, &core.Title{Type: core.TypeMovie, ContentRating: "PG", CollectionName: "Other"}) {
		t.Fatal("label still applies when owner sets a rating")
	}
	if s.Allowed(u, &core.Title{Type: core.TypeMovie, ContentRating: "R", CollectionName: "Kids"}) {
		t.Fatal("owner rating cap")
	}
}

func TestPlexShareLabelUnmapped(t *testing.T) {
	s := shareService(t, PlexShare{FilterMovies: "label=Marvel"})
	u := &core.User{ID: 4}
	if s.Allowed(u, &core.Title{Type: core.TypeMovie, CollectionName: "Marvel"}) != true {
		t.Fatal("collection name maps")
	}
	if s.Allowed(u, &core.Title{Type: core.TypeMovie, CollectionName: "Other"}) {
		t.Fatal("other collections")
	}
	if LabelMapNote == "" {
		t.Fatal("document the unmapped labels")
	}
}

func TestAllowSyncRefusesDownloads(t *testing.T) {
	off := false
	on := true
	s := shareService(t, PlexShare{AllowSync: &off})
	u := &core.User{ID: 5}
	if s.AllowDownloads(u) || experience.DownloadsAllowed(u, true, true, s) {
		t.Fatal("allowSync=0")
	}
	if err := s.Put(context.Background(), 5, Policy{AllowDownloads: &on}); err != nil {
		t.Fatal(err)
	}
	if !s.AllowDownloads(u) || !experience.DownloadsAllowed(u, true, true, s) {
		t.Fatal("owner download override")
	}
	s.SetPlexShare(func(*core.User) PlexShare { return PlexShare{AllowSync: &on} })
	if err := s.Put(context.Background(), 6, Policy{AllowDownloads: &off}); err != nil {
		t.Fatal(err)
	}
	if s.AllowDownloads(&core.User{ID: 6}) {
		t.Fatal("owner refuse wins over allowSync=1")
	}
	owner := &core.User{ID: 1, IsOwner: true}
	if !s.AllowDownloads(owner) {
		t.Fatal("owner always downloads")
	}
}
