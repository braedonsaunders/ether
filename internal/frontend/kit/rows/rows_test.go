package rows

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestSupportedSkipsUnsupportedKinds(t *testing.T) {
	title := &core.Title{Key: 1}
	entries := []frontend.RowEntry{
		{Kind: frontend.RowEntryLiveChannel, LiveChannel: &frontend.RowLiveChannel{ID: "channel"}},
		{Kind: frontend.RowEntryTitle, Title: title, Handle: "issued"},
		{Kind: frontend.RowEntryKind("future")},
	}
	got := Supported(entries, map[frontend.RowEntryKind]bool{frontend.RowEntryTitle: true})
	if len(got) != 1 || got[0].Title != title || got[0].Handle != "issued" {
		t.Fatalf("unexpected supported entries: %+v", got)
	}
}
