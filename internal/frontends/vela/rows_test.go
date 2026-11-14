package vela

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type rowSizeSource struct {
	frontendtest.Experience
	pages []frontend.RowViewPageRequest
}

func (f *rowSizeSource) ListRowViews(context.Context, frontend.RowViewRequest) ([]frontend.RowView, error) {
	rows := make([]frontend.RowView, 10)
	for n := range rows {
		rows[n] = frontend.RowView{ID: fmt.Sprint(n), Title: "Shelf", Kind: "taste", Layout: frontend.RowLayout{Style: "shelf", PreviewOnFocus: true}}
	}
	return rows, nil
}
func (f *rowSizeSource) ResolveRowView(_ context.Context, q frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	f.pages = append(f.pages, q)
	entries := make([]frontend.RowEntry, min(q.Limit, 100-q.Offset))
	for n := range entries {
		entries[n] = frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &frontend.Title{Key: frontend.MovieKey(int64(q.Offset + n + 1)), Title: "Movie"}, Handle: fmt.Sprintf("receipt-%d", q.Offset+n)}
	}
	return frontend.RowViewPage{Entries: entries, ImpressionHandle: "row-receipt", More: q.Offset+len(entries) < 100}, nil
}

type rowSizeImages struct {
	frontend.ImageGrants
	batches []int
}

func (f *rowSizeImages) SignPage(_ context.Context, q frontend.ImagePageRequest) ([]frontend.ImageBatchItem, error) {
	f.batches = append(f.batches, len(q.Titles))
	return nil, nil
}

// Paging must keep the issued source offset even when hero-family exclusions
// require backfill, and conversion cost must grow by page, not by item.
func TestVelaRowSizeAndContinuation(t *testing.T) {
	source := &rowSizeSource{}
	states := []int{}
	source.UserDataFunc = func(_ context.Context, _ *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		states = append(states, len(keys))
		return nil, nil
	}
	images := &rowSizeImages{}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Experience: source, ImageGrants: images}, rowMeta: map[string]frontend.RowView{}}
	viewer := &frontend.Viewer{ID: 1}
	request := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Vela-Device-Id", "device")
		return r
	}
	for _, path := range []string{"/vela/v0/bootstrap", "/vela/v0/rows", "/vela/v0/rows?limit=2"} {
		source.pages = nil
		states = nil
		images.batches = nil
		r := request(path)
		rows, _, err := i.home(r, viewer, "token")
		wantRows := 10
		if r.URL.Query().Get("limit") == "2" {
			wantRows = 2
		}
		if err != nil || len(rows) != wantRows {
			t.Fatalf("%s: rows=%d err=%v", path, len(rows), err)
		}
		if len(source.pages) != wantRows || len(states) != wantRows || len(images.batches) != wantRows {
			t.Fatal("conversion must batch once per row", source.pages, states, images.batches)
		}
		for n, row := range rows {
			if source.pages[n].Limit != 20 || len(row["items"].([]object)) != 20 || states[n] != 20 || images.batches[n] != 20 {
				t.Fatal("wrong per-row page size", row)
			}
			if row["receiptToken"] != "row-receipt" || row["style"] != "shelf" || row["previewOnFocus"] != true {
				t.Fatal("row metadata lost", row)
			}
			cursor, err := i.readRowToken(r, viewer, row["nextCursor"].(string))
			if err != nil || cursor.Offset != 20 {
				t.Fatal("wrong source offset", cursor, err)
			}
		}
	}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 20}, {"?limit=7", 7}, {"?limit=50", 50}, {"?limit=999", 50}, {"?limit=0", 20}, {"?limit=-1", 20}, {"?limit=invalid", 20},
	} {
		r := request("/vela/v0/rows/0" + tc.query)
		r.SetPathValue("id", "0")
		rows, _, err := i.home(r, viewer, "token")
		if err != nil || len(rows) != 1 || len(rows[0]["items"].([]object)) != tc.want {
			t.Fatalf("%s: %v %v", tc.query, rows, err)
		}
		cursor, err := i.readRowToken(r, viewer, rows[0]["nextCursor"].(string))
		if err != nil {
			t.Fatal(err)
		}
		// Simulate the bootstrap's hero exclusion on a subsequent page.
		cursor.Exclude = []string{ids.RatingKeyGUID(frontend.MovieKey(int64(tc.want + 1)))}
		next := request("/vela/v0/rows/0?limit=7&cursor=" + url.QueryEscape(i.sealRowCursor(cursor)))
		next.SetPathValue("id", "0")
		rows, _, err = i.home(next, viewer, "token")
		if err != nil || len(rows[0]["items"].([]object)) != 7 {
			t.Fatal("continuation exclusion lost", rows, err)
		}
		page := source.pages[len(source.pages)-2]
		if page.Offset != tc.want || page.Limit != 7 {
			t.Fatal("continuation page changed", page)
		}
		items := rows[0]["items"].([]object)
		if items[0]["receiptToken"] != fmt.Sprintf("receipt-%d", tc.want+1) {
			t.Fatal("receipt changed", items[0])
		}
		cursor, err = i.readRowToken(next, viewer, rows[0]["nextCursor"].(string))
		if err != nil || cursor.Offset != tc.want+8 || len(cursor.Exclude) != 1 {
			t.Fatal("filtered page changed source cursor", cursor, err)
		}
	}
}

// The offset counts consumed source entries, including excluded heroes, across
// bootstrap backfill and all continuation pages through honest exhaustion.
func TestVelaRowFeaturedBackfill(t *testing.T) {
	source := &rowSizeSource{}
	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i := &instance{seal: seal, host: frontend.Host{Experience: source}, rowMeta: map[string]frontend.RowView{"0": {ID: "0", Title: "Shelf"}}}
	viewer := &frontend.Viewer{ID: 1}
	r := httptest.NewRequest(http.MethodGet, "/vela/v0/bootstrap", nil)
	r.Header.Set("X-Vela-Device-Id", "device")
	excluded := []string{}
	for _, n := range []int64{2, 8, 19} {
		excluded = append(excluded, ids.RatingKeyGUID(frontend.MovieKey(n)))
	}
	row, err := i.rowPage(r, viewer, "0", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	shelf, err := i.rowWire(r, viewer, "token", row, 0, excluded, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(shelf["items"].([]object)) != 17 {
		t.Fatal("fixture did not exclude three featured titles")
	}
	if err := i.backfillShelf(r, viewer, "token", shelf, 20); err != nil {
		t.Fatal(err)
	}
	if len(source.pages) != 2 || source.pages[1].Offset != 20 || source.pages[1].Limit != 3 {
		t.Fatal("backfill fetched beyond the missing slots", source.pages)
	}
	seen := map[string]bool{}
	for page := 0; ; page++ {
		items := shelf["items"].([]object)
		want := 20
		if page == 4 {
			want = 17
		}
		if len(items) != want || shelf["receiptToken"] != "row-receipt" {
			t.Fatalf("page %d: count=%d receipt=%v", page, len(items), shelf["receiptToken"])
		}
		for _, entry := range items {
			id := entry["title"].(object)["id"].(string)
			if seen[id] {
				t.Fatal("duplicate across pages", id)
			}
			for _, hero := range excluded {
				if hero == id {
					t.Fatal("featured title leaked", id)
				}
			}
			seen[id] = true
			key, err := ids.ParseRatingKeyGUID(id)
			if err != nil || entry["receiptToken"] != fmt.Sprintf("receipt-%d", key.TMDB()-1) {
				t.Fatal("entry receipt changed", entry, err)
			}
		}
		if shelf["continuation"] == "exhausted" {
			if len(seen) != 97 || shelf["nextCursor"] != nil {
				t.Fatal("dishonest exhaustion", len(seen), shelf)
			}
			break
		}
		if page >= 4 {
			t.Fatal("source did not exhaust")
		}
		cursor, err := i.readRowToken(r, viewer, shelf["nextCursor"].(string))
		if err != nil || cursor.Offset != 23+page*20 || len(cursor.Exclude) != 3 {
			t.Fatal("wrong consumed source offset or exclusion proof", cursor, err)
		}
		next := httptest.NewRequest(http.MethodGet, "/vela/v0/rows/0?cursor="+url.QueryEscape(shelf["nextCursor"].(string)), nil)
		next.Header = r.Header.Clone()
		next.SetPathValue("id", "0")
		rows, _, err := i.home(next, viewer, "token")
		if err != nil || len(rows) != 1 {
			t.Fatal("continuation failed", rows, err)
		}
		shelf = rows[0]
	}
}
