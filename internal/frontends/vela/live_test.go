package vela

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type livePageFake struct {
	frontend.LiveTV
	channels   []frontend.LiveChannel
	programmes []frontend.LiveProgramme
	snapshot   string
}

func (f *livePageFake) Channels(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
	return f.channels, nil
}
func (f *livePageFake) GuidePage(_ context.Context, _ *frontend.Viewer, q frontend.LiveGuidePageQuery) (frontend.LiveGuidePage, error) {
	if q.Snapshot != "" && q.Snapshot != f.snapshot {
		return frontend.LiveGuidePage{}, frontend.ErrLiveGuideStale
	}
	start := min(q.Offset, len(f.programmes))
	end := start + min(q.Limit, len(f.programmes)-start)
	return frontend.LiveGuidePage{Items: f.programmes[start:end], Snapshot: f.snapshot, More: end < len(f.programmes)}, nil
}
func checkLivePages(t *testing.T) {
	source := &livePageFake{snapshot: "one"}
	now := time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)
	for n := range 251 {
		id := fmt.Sprint(n)
		source.channels = append(source.channels, frontend.LiveChannel{ID: id, Name: id})
		source.programmes = append(source.programmes, frontend.LiveProgramme{ID: id, ChannelID: id, Start: now, End: now.Add(time.Hour)})
	}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Identity: identityFake{}, Experience: frontendtest.Experience{}, LiveTV: source}}
	get := func(path, device string, handler http.HandlerFunc) (int, map[string]json.RawMessage) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer token")
		r.Header.Set("X-Vela-Device-Id", device)
		w := httptest.NewRecorder()
		handler(w, r)
		var page map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return w.Code, page
	}
	for _, which := range []string{"channels", "guide"} {
		base := "/vela/v0/live/" + which + "?limit=100"
		handler := http.HandlerFunc(i.channels)
		if which == "guide" {
			base += "&from=2026-11-14T04:00:00Z&until=2026-11-14T05:00:00Z"
			handler = i.guide
		}
		path := base
		count := 0
		seen := map[string]bool{}
		firstCursor := ""
		for pageNumber := 0; pageNumber < 4; pageNumber++ {
			code, page := get(path, "device", handler)
			if code != 200 {
				t.Fatalf("%s page%d: %d", which, pageNumber, code)
			}
			var items []struct{ ID string }
			var continuation, next string
			_ = json.Unmarshal(page["items"], &items)
			_ = json.Unmarshal(page["continuation"], &continuation)
			_ = json.Unmarshal(page["nextCursor"], &next)
			for _, item := range items {
				if seen[item.ID] {
					t.Fatal("duplicate", item.ID)
				}
				seen[item.ID] = true
				count++
			}
			if continuation == "exhausted" {
				if next != "" {
					t.Fatal("exhausted cursor")
				}
				break
			}
			if continuation != "more" || next == "" {
				t.Fatal("incomplete continuation", continuation)
			}
			if firstCursor == "" {
				firstCursor = next
			}
			path = base + "&cursor=" + url.QueryEscape(next)
		}
		if count != 251 {
			t.Fatalf("%s count%d", which, count)
		}
		if code, _ := get(base+"&cursor="+url.QueryEscape(firstCursor), "other-device", handler); code != 409 {
			t.Fatal("cross-device cursor accepted", code)
		}
		if code, _ := get(strings.Replace(base, "limit=100", "limit=50", 1)+"&cursor="+url.QueryEscape(firstCursor), "device", handler); code != 409 {
			t.Fatal("changed query accepted", code)
		}
		if which == "channels" {
			source.channels[0].Name = "Changed"
		} else {
			source.snapshot = "two"
		}
		if code, _ := get(base+"&cursor="+url.QueryEscape(firstCursor), "device", handler); code != 409 {
			t.Fatal("changed snapshot accepted", code)
		}
	}
}

type rowVisitFake struct {
	frontendtest.Experience
	refreshes int
	lists     int
}

func (f *rowVisitFake) ListRowViews(_ context.Context, q frontend.RowViewRequest) ([]frontend.RowView, error) {
	f.lists++
	id := "stale"
	if q.Refresh {
		f.refreshes++
		id = "fresh"
	}
	return []frontend.RowView{{ID: id}}, nil
}
func (f *rowVisitFake) ResolveRowView(_ context.Context, q frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	if q.ID == "stale" {
		return frontend.RowViewPage{}, frontend.ErrRowViewStale
	}
	return frontend.RowViewPage{}, nil
}
func checkFreshHomeVisit(t *testing.T) {
	source := &rowVisitFake{}
	i := &instance{host: frontend.Host{Experience: source}, rowMeta: map[string]frontend.RowView{}}
	r := httptest.NewRequest(http.MethodGet, "/vela/v0/bootstrap", nil)
	rows, _, err := i.home(r, &frontend.Viewer{ID: 1}, "token")
	if err != nil || source.refreshes != 1 || source.lists != 2 || len(rows) != 1 {
		t.Fatalf("fresh visit rows%d refreshes%d lists%d err%v", len(rows), source.refreshes, source.lists, err)
	}
	r = httptest.NewRequest(http.MethodGet, "/vela/v0/rows/stale", nil)
	r.SetPathValue("id", "stale")
	_, _, err = i.home(r, &frontend.Viewer{ID: 1}, "token")
	if err != frontend.ErrRowViewStale || source.refreshes != 1 {
		t.Fatal("old row silently refreshed", err)
	}
}
