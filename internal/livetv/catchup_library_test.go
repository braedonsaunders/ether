package livetv

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestCatchUpLibraryWindowAndAccess(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(now.UnixMilli())
	s := testService(t, Options{Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
	ch, err := s.SaveChannel(t.Context(), Channel{ID: "archive-library", Name: "Archive", Number: "1", Kind: "iptv", Enabled: true, Private: true, GuideID: "guide", StreamURL: "https://example.invalid/live.ts"})
	if err != nil {
		t.Fatal(err)
	}
	ch.Archive = &ChannelArchive{Days: 2, Mode: "xtream"}
	data, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.DB.ExecContext(t.Context(), "UPDATE livetv_channels SET data=? WHERE id=?", data, ch.ID); err != nil {
		t.Fatal(err)
	}
	for _, hours := range []int{-49, -5, -3, 0} {
		start := now.Add(time.Duration(hours) * time.Hour)
		p := Programme{Start: start, End: start.Add(time.Hour), Item: Item{Title: "News"}}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.o.DB.ExecContext(t.Context(), "INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)", guideSource(ch), ch.GuideID, start.UnixMilli(), p.End.UnixMilli(), raw); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Frontend().(frontend.CatchUpLibrary)
	owner := &core.User{ID: 1, IsOwner: true}
	shows, err := h.CatchUpItems(t.Context(), owner, frontend.CatchUpQuery{})
	if err != nil || len(shows.Items) != 1 {
		t.Fatalf("show grouping: %d, %v", len(shows.Items), err)
	}
	q := frontend.CatchUpQuery{Parent: shows.Items[0].SeasonID}
	episodes, err := h.CatchUpItems(t.Context(), owner, q)
	if err != nil || len(episodes.Items) != 2 || !episodes.Items[0].Programme.Start.Equal(now.Add(-3*time.Hour)) {
		t.Fatalf("archive window or order: %d, %v", len(episodes.Items), err)
	}
	guid := frontend.CatchUpGUID(episodes.Items[0])
	item, err := h.CatchUpItem(t.Context(), owner, guid)
	if err != nil || item.ID != episodes.Items[0].ID {
		t.Fatalf("GUID lookup: %v", err)
	}
	denied := &core.User{ID: 2}
	if visible, err := h.CatchUpLibraryVisible(t.Context(), denied); err != nil || visible {
		t.Fatalf("restricted viewer can see library: %v, %v", visible, err)
	}
	if page, err := h.CatchUpItems(t.Context(), denied, q); err != nil || len(page.Items) != 0 {
		t.Fatalf("restricted viewer can see airings: %d, %v", len(page.Items), err)
	}
	clock.Store(now.Add(44 * time.Hour).UnixMilli())
	page, err := h.CatchUpItems(t.Context(), owner, q)
	if err != nil || len(page.Items) != 2 || page.Items[1].ID != episodes.Items[0].ID {
		t.Fatalf("oldest airing did not expire: %d, %v", len(page.Items), err)
	}
}

func TestCatchUpLibrarySort(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := testService(t, Options{Now: func() time.Time { return now }})
	ch, err := s.SaveChannel(t.Context(), Channel{ID: "archive-sort", Name: "Archive", Number: "1", Kind: "iptv", Enabled: true, GuideID: "guide", StreamURL: "https://example.invalid/live.ts"})
	if err != nil {
		t.Fatal(err)
	}
	ch.Archive = &ChannelArchive{Days: 2, Mode: "xtream"}
	data, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.DB.ExecContext(t.Context(), "UPDATE livetv_channels SET data=? WHERE id=?", data, ch.ID); err != nil {
		t.Fatal(err)
	}
	// Newest first: Morning Show, charlie Report, Alpha Hour, Bravo News.
	for _, p := range []struct {
		hours int
		title string
	}{{-30, "Bravo News"}, {-20, "Alpha Hour"}, {-10, "charlie Report"}, {-2, "Morning Show"}} {
		start := now.Add(time.Duration(p.hours) * time.Hour)
		prog := Programme{Start: start, End: start.Add(time.Hour), Item: Item{Title: p.title}}
		raw, err := json.Marshal(prog)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.o.DB.ExecContext(t.Context(), "INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)", guideSource(ch), ch.GuideID, start.UnixMilli(), prog.End.UnixMilli(), raw); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Frontend().(frontend.CatchUpLibrary)
	owner := &core.User{ID: 1, IsOwner: true}
	titles := func(q frontend.CatchUpQuery) []string {
		t.Helper()
		page, err := h.CatchUpItems(t.Context(), owner, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, item := range page.Items {
			out = append(out, item.Programme.Item.Title)
		}
		return out
	}
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: got %q, want %q", name, got, want)
		}
	}
	check("default", titles(frontend.CatchUpQuery{}), "Morning Show", "charlie Report", "Alpha Hour", "Bravo News")
	check("title", titles(frontend.CatchUpQuery{Sort: frontend.CatchUpSortTitle}), "Alpha Hour", "Bravo News", "charlie Report", "Morning Show")
	check("title desc", titles(frontend.CatchUpQuery{Sort: frontend.CatchUpSortTitleDesc}), "Morning Show", "charlie Report", "Bravo News", "Alpha Hour")
	check("oldest", titles(frontend.CatchUpQuery{Sort: frontend.CatchUpSortOldest}), "Bravo News", "Alpha Hour", "charlie Report", "Morning Show")
	check("title page", titles(frontend.CatchUpQuery{Sort: frontend.CatchUpSortTitle, Offset: 1, Limit: 2}), "Bravo News", "charlie Report")
	page, err := h.CatchUpItems(t.Context(), owner, frontend.CatchUpQuery{Sort: frontend.CatchUpSortTitle, Limit: 1})
	if err != nil || page.Total != 4 || len(page.Items) != 1 {
		t.Fatalf("sorted total: %d of %d, %v", len(page.Items), page.Total, err)
	}
	// Update without advancing the guide epoch to distinguish a cached page
	// read from a rescan. A different viewer must see the current rows.
	if _, err := s.o.DB.ExecContext(t.Context(), "DELETE FROM livetv_programmes WHERE source_id=?", guideSource(ch)); err != nil {
		t.Fatal(err)
	}
	check("cached page", titles(frontend.CatchUpQuery{Sort: frontend.CatchUpSortTitle, Offset: 1, Limit: 2}), "Bravo News", "charlie Report")
	q := frontend.CatchUpQuery{Sort: frontend.CatchUpSortTitle}
	page, err = h.CatchUpItems(t.Context(), &core.User{ID: 2}, q)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("other viewer read cached data: %+v, %v", page, err)
	}
	if _, err := s.o.DB.ExecContext(t.Context(), "UPDATE livetv_channel_epoch SET generation=generation+1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	check("after guide change", titles(q))
}

func TestCatchUpLibrarySearch(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := testService(t, Options{Now: func() time.Time { return now }})
	ch, err := s.SaveChannel(t.Context(), Channel{ID: "archive-search", Name: "Archive", Number: "1", Kind: "iptv", Enabled: true, Private: true, GuideID: "guide", StreamURL: "https://example.invalid/live.ts"})
	if err != nil {
		t.Fatal(err)
	}
	ch.Archive = &ChannelArchive{Days: 2, Mode: "xtream"}
	data, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.DB.ExecContext(t.Context(), "UPDATE livetv_channels SET data=? WHERE id=?", data, ch.ID); err != nil {
		t.Fatal(err)
	}
	for i, title := range []string{"ABC World News Tonight With David Muir", "Paid Programming", "World-News Now"} {
		start := now.Add(-time.Duration(i+1) * time.Hour)
		prog := Programme{Start: start, End: start.Add(30 * time.Minute), Item: Item{Title: title}}
		raw, err := json.Marshal(prog)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.o.DB.ExecContext(t.Context(), "INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)", guideSource(ch), ch.GuideID, start.UnixMilli(), prog.End.UnixMilli(), raw); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Frontend().(frontend.CatchUpLibrary)
	owner := &core.User{ID: 1, IsOwner: true}
	for query, want := range map[string]int{"world news tonight": 1, "WorldNews": 2, "muir": 1, "weather": 0, "!!!": 0, "": 3} {
		page, err := h.CatchUpItems(t.Context(), owner, frontend.CatchUpQuery{Search: query})
		if err != nil || len(page.Items) != want || page.Total != want {
			t.Fatalf("search %q: %d items (total %d), want %d, %v", query, len(page.Items), page.Total, want, err)
		}
	}
	if page, err := h.CatchUpItems(t.Context(), &core.User{ID: 2}, frontend.CatchUpQuery{Search: "world"}); err != nil || len(page.Items) != 0 {
		t.Fatalf("restricted viewer finds airings: %d, %v", len(page.Items), err)
	}
	// The title and episode title are readable. A nonmatching row must be
	// skipped before its remaining fields are decoded as a Programme.
	if _, err := s.o.DB.ExecContext(t.Context(), "UPDATE livetv_programmes SET data=? WHERE start_ms=?",
		`{"item":{"title":"Paid Programming","durationMs":"invalid"}}`, now.Add(-2*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	page, err := h.CatchUpItems(t.Context(), owner, frontend.CatchUpQuery{Search: "world-news now"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Programme.Item.Title != "World-News Now" {
		t.Fatalf("search decoded a nonmatching row: %+v, %v", page, err)
	}
}
