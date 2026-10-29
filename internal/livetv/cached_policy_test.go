package livetv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type policyCatalog struct {
	core.Catalog
	calls  int
	titles map[core.RatingKey]*core.Title
}

func (c *policyCatalog) Get(_ context.Context, key core.RatingKey) (*core.Title, error) {
	c.calls++
	t := c.titles[key]
	if t == nil {
		return nil, core.ErrNotFound
	}
	return t, nil
}

func TestCachedChannelPolicyIsScopedAndLocal(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(100, 0)
	episode := core.EpisodeKey(7, 1, 1)
	show := core.ShowKey(7)
	titles := map[core.RatingKey]*core.Title{episode: {Key: episode, Type: core.TypeEpisode, ShowKey: show, ContentRating: "PG"}, show: {Key: show, Type: core.TypeShow, ContentRating: "PG"}}
	network := &policyCatalog{titles: titles}
	cacheCalls := 0
	cached := func(_ context.Context, k core.RatingKey) (*core.Title, error) {
		cacheCalls++
		if titles[k] == nil {
			return nil, core.ErrNotFound
		}
		return titles[k], nil
	}
	service := testService(t, Options{Catalog: network, Now: func() time.Time { return now }})
	for i := 0; i < 100; i++ {
		c := testChannel()
		c.ID = fmt.Sprintf("channel-%03d", i)
		c.Items = []Item{{Key: episode, Title: "Episode", DurationMS: 60000, Rating: "PG"}}
		if _, err := service.SaveChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	viewer := &core.User{ID: 7, MaxContentRating: "PG", AllowedSections: []int{2}}
	channels, err := service.AllowedChannels(ctx, viewer)
	if err != nil || len(channels) != 100 {
		t.Fatalf("legacy allowed %d %v", len(channels), err)
	}
	network.calls = 0
	for i := 0; i < 10; i++ {
		if err := service.AllowedChannelCached(ctx, viewer, "channel-000", cached); err != nil {
			t.Fatal(err)
		}
	}
	if network.calls != 0 || cacheCalls != 20 {
		t.Fatalf("network=%d cached=%d, want 0 and 20", network.calls, cacheCalls)
	}
	titles[show].ContentRating = "R"
	if err := service.AllowedChannelCached(ctx, viewer, "channel-000", cached); err == nil {
		t.Fatal("restricted inherited show allowed")
	}
	titles[show].ContentRating = "PG"
	delete(titles, show)
	if err := service.AllowedChannelCached(ctx, viewer, "channel-000", cached); err == nil {
		t.Fatal("missing parent allowed")
	}
	titles[show] = &core.Title{Key: show, Type: core.TypeShow, ContentRating: "PG"}
	if err := service.SavePolicy(ctx, Policy{UserID: viewer.ID, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := service.AllowedChannelCached(ctx, viewer, "channel-000", cached); err == nil {
		t.Fatal("revoked live policy allowed")
	}
	if network.calls != 0 {
		t.Fatalf("remote metadata calls: %d", network.calls)
	}
}
