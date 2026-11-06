package state

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestHistoryPersistentIdentityAndKinds(t *testing.T) {
	s, now := newTestState(t)
	ctx := WithOrigin(context.Background(), Origin{DeviceID: 7})
	key := core.MovieKey(1)
	if err := s.Scrobble(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	first, err := s.QueryHistory(ctx, core.HistoryQuery{UserID: 1, ViewType: 1, Limit: 20})
	if err != nil || len(first) != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	if first[0].ID == 0 || first[0].DeviceID != 7 {
		t.Fatalf("identity lost: %+v", first[0])
	}
	if err := s.DeleteHistory(ctx, 2, first[0].ID); err == nil {
		t.Fatal("cross-viewer delete accepted")
	}
	if err := s.DeleteHistory(ctx, 1, first[0].ID); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Millisecond)
	if err := s.Scrobble(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	newer, _ := s.QueryHistory(ctx, core.HistoryQuery{UserID: 1, ViewType: 1, Limit: 20})
	if len(newer) != 1 || newer[0].ID <= first[0].ID {
		t.Fatalf("ID reused: %+v", newer)
	}
	empty, _ := s.QueryHistory(ctx, core.HistoryQuery{UserID: 1, ViewType: 0, Limit: 20})
	if len(empty) != 0 {
		t.Fatal("manual scrobble became playback history")
	}
	// Same-second order is the stable primary-key order for either direction.
	if err := s.Scrobble(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	for _, asc := range []bool{false, true} {
		rows, err := s.QueryHistory(ctx, core.HistoryQuery{UserID: 1, ViewType: 1, Asc: asc, Limit: 20})
		if err != nil || len(rows) != 2 || rows[0].ID >= rows[1].ID {
			t.Fatalf("tie order %+v %v", rows, err)
		}
	}
	sec := now.Unix()
	rows, err := s.QueryHistory(ctx, core.HistoryQuery{UserID: 1, ViewType: 1, Since: &sec, Until: &sec, At: &sec, Limit: 20})
	if err != nil || len(rows) != 2 {
		t.Fatalf("inclusive seconds %+v %v", rows, err)
	}
}
