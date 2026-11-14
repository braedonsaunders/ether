package livetv

import (
	"context"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// The invariant is membership rather than duplicated channels: ordered overlaps,
// rename-stable identity, and viewer visibility hold at the same boundary.
func TestOwnerGuideMembership(t *testing.T) {
	ctx := context.Background()
	s := testService(t, Options{})
	for _, c := range []Channel{
		{ID: "news", Name: "News", Number: "1", Kind: "iptv", Enabled: true, SourceID: "s", Group: "News", StreamURL: "https://example.invalid/news"},
		{ID: "kids", Name: "Kids", Number: "2", Kind: "iptv", Enabled: true, SourceID: "s", Group: "Kids", StreamURL: "https://example.invalid/kids"},
		{ID: "off", Name: "Off", Number: "3", Kind: "iptv", Enabled: false, SourceID: "s", Group: "News", StreamURL: "https://example.invalid/off"},
	} {
		if _, err := s.SaveChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	gs := []OwnerGuide{{ID: "family", Name: "Family", Groups: []GuideGroup{{"s", "News"}, {"s", "Kids"}}}, {ID: "news", Name: "News", Groups: []GuideGroup{{"s", "News"}}}, {ID: "hidden", Name: "Hidden", All: true, Hidden: true}}
	if err := s.SaveOwnerGuides(ctx, gs); err != nil {
		t.Fatal(err)
	}
	host := liveTVHost{s: s}
	viewer := &core.User{ID: 1, IsOwner: true}
	cs, err := host.Channels(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("visible channels: %+v", cs)
	}
	for _, c := range cs {
		if c.ID == "news" && !reflect.DeepEqual(c.Guides, []frontend.LiveGuide{{ID: "family", Name: "Family"}, {ID: "news", Name: "News"}}) {
			t.Fatalf("memberships: %+v", c.Guides)
		}
	}
	selected, err := host.GuideChannels(ctx, viewer, "news")
	if err != nil || len(selected) != 1 || selected[0].ID != "news" {
		t.Fatalf("guide: %+v %v", selected, err)
	}
	gs[0].Name = "Renamed"
	gs[0], gs[1] = gs[1], gs[0]
	if err := s.SaveOwnerGuides(ctx, gs); err != nil {
		t.Fatal(err)
	}
	guides, err := host.Guides(ctx, viewer)
	if err != nil || len(guides) != 2 || guides[1].ID != "family" || guides[1].Name != "Renamed" {
		t.Fatalf("renamed order: %+v %v", guides, err)
	}
	if _, err := host.GuideChannels(ctx, viewer, "hidden"); err == nil {
		t.Fatal("hidden guide exposed")
	}
}
