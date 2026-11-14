package vela

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http/httptest"
	"testing"
)

func TestRankedRowPresentationPreservesReceipts(t *testing.T) {
	source := frontendtest.Experience{UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return nil, nil
	}}
	inst := &instance{host: frontend.Host{Experience: source}}
	entries := make([]frontend.RowEntry, 12)
	for n := range entries {
		entries[n] = frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &frontend.Title{Key: frontend.MovieKey(int64(n + 1)), Type: frontend.TypeMovie, Title: "Movie"}, Handle: fmt.Sprintf("receipt-%d", n)}
	}
	for _, ranked := range []bool{false, true} {
		row := frontend.Row{ID: "stable", Title: "Top 10", RankedTop10: ranked, Entries: entries, Handle: "row-receipt", More: true, PreviewOnFocus: true}
		wire, err := inst.rowWire(httptest.NewRequest("GET", "/vela/v0/rows", nil), &frontend.Viewer{ID: 1}, "token", row, 0, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		items := wire["items"].([]object)
		want := 12
		if ranked {
			want = 10
		}
		if len(items) != want || wire["receiptToken"] != "row-receipt" || wire["previewOnFocus"] != true {
			t.Fatal(wire)
		}
		if ranked && (wire["presentation"] != "ranked" || wire["continuation"] != "exhausted") {
			t.Fatal(wire)
		}
		if !ranked {
			if _, ok := wire["presentation"]; ok {
				t.Fatal("ordinary row became ranked")
			}
		}
		for n, item := range items {
			if item["id"] != ids.RatingKeyGUID(entries[n].Title.Key) || item["receiptToken"] != entries[n].Handle {
				t.Fatal("changed item identity/order/receipt", item)
			}
		}
	}
}
