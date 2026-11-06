package model

import (
	"strings"
	"testing"
)

func TestHistoryProjectionHasIndependentOrder(t *testing.T) {
	row := HistoryMetadata{HistoryKey: "/status/sessions/history/7", LibrarySectionID: "2", Title: "Episode", GrandparentTitle: "Show", Type: "episode", Index: Ptr(1), ParentIndex: Ptr(2), OriginallyAvailableAt: "2026-01-01", ViewedAt: 42, AccountID: 3, DeviceID: 9, ViewType: Ptr(1)}
	b, err := Marshal(&MediaContainer{Size: 1, History: []HistoryMetadata{row}}, XML)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	previous := -1
	for _, name := range []string{"historyKey", "librarySectionID", "title", "grandparentTitle", "type", "index", "parentIndex", "originallyAvailableAt", "viewedAt", "accountID", "deviceID", "viewType"} {
		next := strings.Index(text, " "+name+"=")
		if next <= previous {
			t.Fatalf("%s out of order: %s", name, text)
		}
		previous = next
	}
	for _, name := range []string{" key=", " ratingKey=", " thumb=", " summary="} {
		if strings.Contains(text, name) {
			t.Fatalf("unexpected %s: %s", name, text)
		}
	}
}
