package livetv

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// TestGuideForChannelsIsSetBased pins the guide to a bounded number of
// statements: the old loop asked once per enabled channel.
func TestGuideForChannelsIsSetBased(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	ids := make([]string, 12)
	for i := range ids {
		ids[i] = fmt.Sprintf("g%d", i)
	}
	if _, err := s.ImportSource(ctx, Source{ID: "src", Name: "IPTV", Playlist: m3uFixture(
		"g0=One", "g1=Two", "g2=Three", "g3=Four", "g4=Five", "g5=Six",
		"g6=Seven", "g7=Eight", "g8=Nine", "g9=Ten", "g10=Eleven", "g11=Twelve",
	), Guide: xmltvFixture(10, ids...)}); err != nil {
		t.Fatal(err)
	}
	channels, err := s.Channels(ctx)
	if err != nil || len(channels) != 12 {
		t.Fatalf("channels: %v %d", err, len(channels))
	}
	want := make([]string, 0, len(channels))
	for _, c := range channels {
		want = append(want, c.ID)
	}
	statsCtx, stats := store.WithQueryStats(ctx)
	ps, err := s.GuideForChannels(statsCtx, ownerUser(), guideTestNow, guideTestNow.Add(2*time.Hour), want)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 24 {
		t.Fatalf("programmes for %d channels: got %d, want 24", len(want), len(ps))
	}
	seen := map[string]bool{}
	for _, p := range ps {
		seen[p.ChannelID] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("missing programmes for %s", id)
		}
	}
	if q := stats.Queries(); q > 3 {
		t.Fatalf("guide used %d queries for %d channels", q, len(want))
	}
	all, err := s.Guide(ctx, ownerUser(), guideTestNow, guideTestNow.Add(2*time.Hour))
	if err != nil || len(all) != 24 {
		t.Fatalf("all-channel guide: %v %d", err, len(all))
	}
}

// TestAllowedChannelsBatchesCurrentProgramme pins the on-now read to one
// statement and keeps the latest start when guide rows overlap, which is what
// the old per-channel programmeAt chose.
func TestAllowedChannelsBatchesCurrentProgramme(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	guide := `<tv>
<channel id="a"><display-name>A</display-name></channel>
<channel id="b"><display-name>B</display-name></channel>
<programme channel="a" start="20261019100000 +0000" stop="20261019140000 +0000"><title>A long</title><rating><value>G</value></rating></programme>
<programme channel="a" start="20261019113000 +0000" stop="20261019123000 +0000"><title>A short</title><rating><value>R</value></rating></programme>
<programme channel="b" start="20261019100000 +0000" stop="20261019140000 +0000"><title>B long</title><rating><value>G</value></rating></programme>
</tv>`
	if _, err := s.ImportSource(ctx, Source{ID: "src", Name: "IPTV", Playlist: m3uFixture("a=A", "b=B"), Guide: guide}); err != nil {
		t.Fatal(err)
	}
	channels, err := s.Channels(ctx)
	if err != nil || len(channels) != 2 {
		t.Fatalf("channels: %v %d", err, len(channels))
	}
	byGuide := map[string]string{}
	for _, c := range channels {
		byGuide[c.GuideID] = c.ID
	}

	statsCtx, stats := store.WithQueryStats(ctx)
	airing, err := s.programmesAt(statsCtx, channels, guideTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if q := stats.Queries(); q != 1 {
		t.Fatalf("current programmes used %d queries, want 1", q)
	}
	if p := airing[0]; p.Item.Title != "A short" {
		t.Fatalf("overlapping programme: got %q, want the latest start", p.Item.Title)
	}
	if p := airing[1]; p.Item.Title != "B long" {
		t.Fatalf("single programme: got %q", p.Item.Title)
	}

	// The R-rated overlap hides channel A from a G viewer; channel B stays.
	viewer := &core.User{ID: 7, MaxContentRating: "G"}
	statsCtx, stats = store.WithQueryStats(ctx)
	allowed, err := s.AllowedChannels(statsCtx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 1 || allowed[0].GuideID != "b" {
		t.Fatalf("restricted lineup: %+v", allowed)
	}
	if q := stats.Queries(); q > 3 {
		t.Fatalf("allowed channels used %d queries for %d channels", q, len(channels))
	}
	if _, err := s.AllowedChannel(ctx, viewer, byGuide["a"]); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("restricted channel permitted: %v", err)
	}
	// Once the latest-starting overlap ends, both listing and tune see no
	// current programme, rather than resurrecting the older long programme.
	clock.now = guideTestNow.Add(time.Hour)
	listed, err := s.AllowedChannels(ctx, viewer)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range channels {
		_, tuneErr := s.AllowedChannel(ctx, viewer, c.ID)
		present := false
		for _, visible := range listed {
			present = present || visible.ID == c.ID
		}
		if present != (tuneErr == nil) {
			t.Fatalf("lineup/tune disagreement for %s: %v", c.ID, tuneErr)
		}
	}

	c, err := s.AllowedChannel(ctx, ownerUser(), channels[0].Number)
	if err != nil || c.ID != channels[0].ID {
		t.Fatalf("resolve by number: %v %+v", err, c)
	}
}

// TestChannelPagePaginatesAndFilters covers the console's server-side page:
// natural order, search, filters, counts and coverage without loading the
// whole lineup.
func TestChannelPagePaginatesAndFilters(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	for i := 1; i <= 25; i++ {
		c := Channel{
			ID:        fmt.Sprintf("c%02d", i),
			Number:    fmt.Sprint(i),
			Name:      fmt.Sprintf("Alpha %02d", i),
			Kind:      "iptv",
			Enabled:   i%5 != 0,
			Favorite:  i%7 == 0,
			SourceID:  "src",
			StreamURL: "https://owner.invalid/" + fmt.Sprint(i) + ".m3u8",
		}
		if i%2 == 0 {
			c.Group = "News"
		}
		if _, err := s.SaveChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ChannelPage(ctx, ChannelPageQuery{Filter: "all", Limit: 10, Offset: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 25 || len(page.Channels) != 10 {
		t.Fatalf("page: total=%d rows=%d", page.Total, len(page.Channels))
	}
	if page.Channels[0].Number != "11" || page.Channels[9].Number != "20" {
		t.Fatalf("natural order: %q..%q", page.Channels[0].Number, page.Channels[9].Number)
	}
	if page.Counts.All != 25 || page.Counts.Live != 20 || page.Counts.Off != 5 || page.Counts.Missing != 20 {
		t.Fatalf("counts: %+v", page.Counts)
	}
	if page.NextNumber != "26" {
		t.Fatalf("next number: %q", page.NextNumber)
	}
	live, err := s.ChannelPage(ctx, ChannelPageQuery{Filter: "live", Limit: 100})
	if err != nil || live.Total != 20 {
		t.Fatalf("live filter: %v %d", err, live.Total)
	}
	favs, err := s.ChannelPage(ctx, ChannelPageQuery{Filter: "favorites", Limit: 100})
	if err != nil || favs.Total != 3 {
		t.Fatalf("favorites: %v %d", err, favs.Total)
	}
	search, err := s.ChannelPage(ctx, ChannelPageQuery{Filter: "all", Q: "alpha 07", Limit: 100})
	if err != nil || search.Total != 1 || search.Channels[0].ID != "c07" {
		t.Fatalf("search: %v %+v", err, search.Channels)
	}
	news, err := s.ChannelPage(ctx, ChannelPageQuery{Filter: "live", Group: "News", Limit: 100})
	if err != nil || news.Total != 10 || news.Channels[0].ID != "c02" {
		t.Fatalf("group: %v %d", err, news.Total)
	}
	missing, err := s.ChannelPage(ctx, ChannelPageQuery{Filter: "missing", Limit: 100})
	if err != nil || missing.Total != 20 {
		t.Fatalf("missing: %v %d", err, missing.Total)
	}
	if got, ok := page.Coverage["c11"]; !ok || got {
		t.Fatalf("coverage for unmapped channel: %v %v", got, ok)
	}
	groups := page.Groups
	if len(groups) != 1 || groups[0].Count != 25 || groups[0].SourceID != "src" || groups[0].Start != 0 {
		t.Fatalf("groups: %+v", groups)
	}
}

func TestOwnerChannelPageAndGuideFilterRoutes(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: guideTestNow}
	s := testService(t, Options{Now: clock.Now})
	for i := 1; i <= 4; i++ {
		c := Channel{
			ID: fmt.Sprintf("r%d", i), Number: fmt.Sprint(i), Name: fmt.Sprintf("Route %d", i),
			Kind: "iptv", Enabled: true, SourceID: "src",
			StreamURL: fmt.Sprintf("https://owner.invalid/%d.m3u8", i),
		}
		if _, err := s.SaveChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ImportSource(ctx, Source{ID: "epg", Name: "EPG", Guide: xmltvFixture(3, "route.one=Route 1")}); err != nil {
		t.Fatal(err)
	}
	var page ChannelPage
	if code := ownerJSON(t, s, "GET", "/channels/page?limit=2&offset=1&filter=all", "", &page); code != 200 {
		t.Fatalf("page route: %d", code)
	}
	if page.Total != 4 || len(page.Channels) != 2 || page.Channels[0].Number != "2" {
		t.Fatalf("page route body: %+v", page)
	}
	var one Channel
	if code := ownerJSON(t, s, "GET", "/channels/r3", "", &one); code != 200 || one.ID != "r3" {
		t.Fatalf("single channel route: %d %+v", code, one)
	}
	var guide []Programme
	if code := ownerJSON(t, s, "GET", "/guide?from="+guideTestNow.Format(time.RFC3339)+"&until="+guideTestNow.Add(2*time.Hour).Format(time.RFC3339)+"&channels=r1", "", &guide); code != 200 {
		t.Fatalf("guide route: %d", code)
	}
	if len(guide) != 0 {
		t.Fatalf("unmapped channel gained programmes: %+v", guide)
	}

	// The owner API negotiates gzip for the multi-megabyte legacy payloads.
	req := httptest.NewRequest("GET", "/channels", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip response: %d %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	var all []Channel
	if err := json.NewDecoder(zr).Decode(&all); err != nil || len(all) != 4 {
		t.Fatalf("gzip body: %v %d", err, len(all))
	}
}

// Independent service instances share the DB epoch, not process-local events.
// A write on one cluster node must invalidate another node's summary and
// coverage caches, including the missing-guide filter.
func TestChannelPageInvalidatesAcrossServices(t *testing.T) {
	ctx := context.Background()
	first := testService(t, Options{Now: func() time.Time { return guideTestNow }})
	second := testService(t, Options{DB: first.o.DB, Now: func() time.Time { return guideTestNow }})
	src := Source{ID: "shared", Name: "Shared", Playlist: m3uFixture("a=Before")}
	if _, err := first.ImportSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	before, err := second.ChannelPage(ctx, ChannelPageQuery{Filter: "missing"})
	if err != nil || before.Total != 1 || before.Channels[0].Name != "Before" {
		t.Fatalf("initial page: %+v %v", before, err)
	}
	src.Playlist = m3uFixture("a=After", "b=New")
	src.Guide = xmltvFixture(3, "a", "b")
	if _, err := first.ImportSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	after, err := second.ChannelPage(ctx, ChannelPageQuery{})
	if err != nil || after.Total != 2 || after.Channels[0].Name != "After" || after.Counts.Missing != 0 {
		t.Fatalf("stale page after remote import: %+v %v", after, err)
	}
	summaries, err := second.ChannelSummaries(ctx)
	if err != nil || len(summaries) != 2 || summaries[0].Name != "After" {
		t.Fatalf("stale summaries: %+v %v", summaries, err)
	}
}

func TestSummaryGuideRetainsHealthSweepPolicy(t *testing.T) {
	ctx := context.Background()
	s := testService(t, Options{Now: func() time.Time { return guideTestNow }})
	if _, err := s.ImportSource(ctx, Source{ID: "health", Name: "Health", Playlist: m3uFixture("a=A"), Guide: xmltvFixture(3, "a")}); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Channels(ctx)
	c := cs[0]
	// Prime the summary, then update health without rebuilding its cached identity.
	if _, err := s.ChannelSummaries(ctx); err != nil {
		t.Fatal(err)
	}
	s.healthMu.Lock()
	s.channelHealth = map[string]sweepResult{c.ID: {signature: sweepSignature(c), available: false}}
	s.healthMu.Unlock()
	viewer := &core.User{ID: 7}
	for _, u := range []*core.User{viewer, ownerUser()} {
		for _, span := range []time.Duration{time.Second, 2 * time.Hour} {
			for _, ids := range [][]string{nil, {c.ID}} {
				ps, err := s.GuideForChannels(ctx, u, guideTestNow, guideTestNow.Add(span), ids)
				if err != nil {
					t.Fatal(err)
				}
				if u.IsOwner && len(ps) == 0 || !u.IsOwner && len(ps) != 0 {
					t.Fatalf("guide owner=%v span=%s ids=%v programmes=%d", u.IsOwner, span, ids, len(ps))
				}
			}
		}
		allowed, err := s.AllowedChannels(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if u.IsOwner && len(allowed) != 1 || !u.IsOwner && len(allowed) != 0 {
			t.Fatal("lineup health policy")
		}
		_, ps, err := s.OnNow(ctx, u, guideTestNow, guideTestNow.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if u.IsOwner && len(ps) == 0 || !u.IsOwner && len(ps) != 0 {
			t.Fatal("on-now health policy")
		}
		rec := httptest.NewRecorder()
		s.ServeXMLTV(rec, httptest.NewRequest("GET", "/guide.xml", nil), u)
		if rec.Code != 200 || (!u.IsOwner && strings.Contains(rec.Body.String(), "<programme")) {
			t.Fatal("XMLTV health policy", rec.Code)
		}
	}
}

func TestAllowedChannelNumberSkipsUnavailableMatches(t *testing.T) {
	ctx := context.Background()
	s := testService(t, Options{})
	for i, c := range []Channel{
		{ID: "a", Kind: "iptv", Enabled: false, Group: "Disabled"},
		{ID: "b", Kind: "iptv", Enabled: true, Private: true, Group: "Private"},
		{ID: "c", Kind: "iptv", Enabled: true, Group: "Visible"},
	} {
		c.Number = "1"
		c.Name = c.ID
		c.StreamURL = fmt.Sprintf("https://example.invalid/%d.ts", i)
		if _, err := s.SaveChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.AllowedChannel(ctx, &core.User{ID: 7}, "1")
	if err != nil || c.ID != "c" {
		t.Fatalf("number resolve: %+v %v", c, err)
	}
}

func TestGuideOnlyImportInvalidatesRemoteCoverage(t *testing.T) {
	ctx := context.Background()
	first := testService(t, Options{Now: func() time.Time { return guideTestNow }})
	second := testService(t, Options{DB: first.o.DB, Now: first.o.Now})
	c := Channel{ID: "guide-only-channel", Name: "Channel", Number: "1", Kind: "iptv", Enabled: true, GuideSourceID: "epg", GuideID: "a", StreamURL: "https://example.invalid/live.ts"}
	if _, err := first.SaveChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	before, err := second.ChannelPage(ctx, ChannelPageQuery{Filter: "missing"})
	if err != nil || before.Total != 1 {
		t.Fatal(before, err)
	}
	if _, err = first.ImportSource(ctx, Source{ID: "epg", Name: "Guide", Guide: xmltvFixture(3, "a")}); err != nil {
		t.Fatal(err)
	}
	after, err := second.ChannelPage(ctx, ChannelPageQuery{Filter: "missing"})
	if err != nil || after.Total != 0 {
		t.Fatal(after, err)
	}
}

func TestOwnerGuideChannelFilterRemainsBounded(t *testing.T) {
	s := testService(t, Options{})
	ids := make([]string, 4097)
	for i := range ids {
		ids[i] = fmt.Sprintf("channel-%d", i)
	}
	if code := ownerJSON(t, s, "GET", "/guide?channels="+strings.Join(ids, ","), "", nil); code != 400 {
		t.Fatalf("owner guide cap: %d", code)
	}
}
