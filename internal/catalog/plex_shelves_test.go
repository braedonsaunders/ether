package catalog

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	_ "github.com/braedonsaunders/ether/internal/state"
	"testing"
	"time"
)

func TestPlexShelfDeepPageAndCount(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Now().UTC().Truncate(time.Second)
	c.SetNow(func() time.Time { return now })
	for i := 0; i < 250; i++ {
		title := core.Title{Key: core.MovieKey(int64(1000 + i)), Type: core.TypeMovie, Title: "Shelf", Released: now.Add(-time.Duration(i+1) * time.Hour)}
		if err := c.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
		count := 0
		if i < 230 {
			count = 1
		}
		if _, err := c.db.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_offset_ns,view_count,last_viewed_at,updated_at) VALUES(?,?,?,?,?,?)`, 1, int64(title.Key), time.Minute.Nanoseconds(), count, now.Add(-time.Duration(i)*time.Second).UnixNano(), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := c.PlexRecentPage(ctx, core.BrowseQuery{Filter: core.Filter{KnownAddedDate: true}, Offset: 220, Limit: 7}, []core.MediaType{core.TypeMovie})
	if err != nil || recent.Total != 250 || len(recent.Items) != 7 || recent.Items[0].Key != core.MovieKey(1220) {
		t.Fatalf("recent deep page %+v %v", recent, err)
	}
	deck, err := c.PlexOnDeckPage(ctx, 1, 220, 7, core.Filter{}, false)
	if err != nil || deck.Total != 250 || len(deck.Items) != 7 || deck.Items[0].Key != core.MovieKey(1220) {
		t.Fatalf("onDeck deep page %+v %v", deck, err)
	}
	unwatched := true
	recent, err = c.PlexRecentPage(ctx, core.BrowseQuery{Filter: core.Filter{KnownAddedDate: true, Group: &core.FilterGroup{Unwatched: &unwatched, UserID: 1}}, Offset: 0, Limit: 3}, []core.MediaType{core.TypeMovie})
	if err != nil || recent.Total != 20 || len(recent.Items) != 3 || recent.Items[0].Key != core.MovieKey(1230) {
		t.Fatalf("filter before page %+v %v", recent, err)
	}
	deck, err = c.PlexOnDeckPage(ctx, 2, 0, 7, core.Filter{}, false)
	if err != nil || deck.Total != 0 || len(deck.Items) != 0 {
		t.Fatal("viewer leak", deck, err)
	}
}

func TestPlexOnDeckCurrentShowAnchorAndHide(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	now := time.Now().UTC().Truncate(time.Second)
	c.SetNow(func() time.Time { return now })
	show := core.ShowKey(700)
	title := core.Title{Key: show, Type: core.TypeShow, Title: "Show", Released: now.Add(-time.Hour)}
	if err := c.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		ep := core.Title{Key: core.EpisodeKey(700, 1, i), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: i, Title: "Episode", Released: now.Add(-time.Hour)}
		if err := c.UpsertTitle(ctx, ep, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []int{4, 1} {
		at := now
		if i == 4 {
			at = now.Add(-time.Minute)
		}
		if _, err := c.db.ExecContext(ctx, `INSERT INTO state_watch(user_id,rating_key,view_count,last_viewed_at) VALUES(?,?,1,?)`, 1, int64(core.EpisodeKey(700, 1, i)), at.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.PlexOnDeckPage(ctx, 1, 0, 10, core.Filter{}, false)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != core.EpisodeKey(700, 1, 2) {
		t.Fatalf("show anchor %+v %v", page, err)
	}
	visible := core.Filter{ExcludeKeys: []core.RatingKey{core.EpisodeKey(700, 1, 2)}}
	page, err = c.PlexOnDeckPage(ctx, 1, 0, 10, visible, false)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != core.EpisodeKey(700, 1, 3) {
		t.Fatal("blocked next episode was not skipped", page, err)
	}
	visible.ExcludeKeys = []core.RatingKey{core.EpisodeKey(700, 1, 1), core.EpisodeKey(700, 1, 4)}
	page, err = c.PlexOnDeckPage(ctx, 1, 0, 10, visible, false)
	if err != nil || page.Total != 0 {
		t.Fatal("blocked episode established watch anchor", page, err)
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO state_continue_hidden(user_id,item_key) VALUES(1,?)`, int64(show)); err != nil {
		t.Fatal(err)
	}
	page, err = c.PlexOnDeckPage(ctx, 1, 0, 10, core.Filter{}, false)
	if err != nil || page.Total != 0 {
		t.Fatal("hidden show", page, err)
	}
}
