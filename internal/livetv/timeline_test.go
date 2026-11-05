package livetv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestTimelineCoversRangeWithoutOverlaps(t *testing.T) {
	c := testChannel()
	a := time.Unix(100, 0)
	ps := []Programme{
		{ID: "a", Start: a.Add(-time.Second), End: a.Add(10 * time.Second), Item: Item{Key: 1, Title: "Programme"}},
		{ID: "b", Start: a.Add(9 * time.Second), End: a.Add(20 * time.Second), Item: Item{Slate: "reel", ReelClipID: "clip", Title: "Spot"}},
		{ID: "c", Start: a.Add(20 * time.Second), End: a.Add(25 * time.Second), Item: Item{Slate: "back", Title: "Back"}},
		{ID: "d", Start: a.Add(30 * time.Second), End: a.Add(time.Minute), Item: Item{Key: 2, Title: "Next"}},
	}
	timeline := buildTimeline(c, ps, a.UnixMilli(), a.Add(45*time.Second).UnixMilli(), a)
	cursor := a.UnixMilli()
	pods := 0
	for _, e := range timeline.Elements {
		if e.Start != cursor || e.End <= e.Start {
			t.Fatalf("invalid element after %d: %+v", cursor, e)
		}
		cursor = e.End
		if e.Kind == "pod" {
			pods++
			child := e.Start
			for _, spot := range e.Children {
				if spot.Start != child {
					t.Fatal("pod child gap")
				}
				child = spot.End
			}
			if child != e.End {
				t.Fatal("pod uncovered")
			}
		}
	}
	if cursor != a.Add(45*time.Second).UnixMilli() || pods != 1 {
		t.Fatalf("coverage ended %d pods %d", cursor, pods)
	}
}

func TestTimelineColdSnapshot(t *testing.T) {
	s := testService(t, Options{})
	c := testChannel()
	raw, _ := json.Marshal(c)
	if _, err := s.o.DB.ExecContext(context.Background(), `INSERT INTO livetv_channels(id,data) VALUES(?,?)`, c.ID, raw); err != nil {
		t.Fatal(err)
	}
	cold := &Service{o: s.o}
	r := httptest.NewRequest("GET", "/channels/"+c.ID+"/timeline?from=100000&to=110000", nil)
	w := httptest.NewRecorder()
	cold.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("cold snapshot: %d %s", w.Code, w.Body)
	}
}

// Real saved fixture shapes exercise migration without depending on live
// catalogue contents. Schedule IDs and boundaries must remain byte identical.
func TestProgrammingMigrationPreservesSchedules(t *testing.T) {
	ctx := context.Background()
	s := testService(t, Options{})
	base := testChannel()
	base.Blocks = []Block{{Start: 480, End: 600, Items: base.Items, Shuffle: true}}
	fixtures := []Channel{testChannel(), base}
	modern := base
	modern.ID = "weekly"
	modern.Mode = "shuffle"
	modern.Timezone = "America/Toronto"
	modern.Blocks = append([]Block(nil), base.Blocks...)
	modern.Blocks[0].Days = []int{1, 2, 3, 4, 5}
	fixtures = append(fixtures, modern)
	mixed := modern
	mixed.ID = "mix"
	mixed.Blocks = append([]Block(nil), modern.Blocks...)
	mixed.Blocks[0].Sources = []ProgramSource{{Weight: 2, Items: base.Items[:1]}, {Weight: 1, Items: base.Items[1:]}}
	fixtures = append(fixtures, mixed)
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for i, c := range fixtures {
		c.ID += string(rune('a' + i))
		raw, _ := json.Marshal(c)
		if _, err := s.o.DB.ExecContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?)`, c.ID, raw); err != nil {
			t.Fatal(err)
		}
		fixtures[i] = c
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateProgramming(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, before := range fixtures {
		var after Channel
		var raw []byte
		if err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_channels WHERE id=?`, before.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &after); err != nil {
			t.Fatal(err)
		}
		// The original scheduler consumed the stored flattened Items; source
		// declarations only participated when the channel was saved.
		baseline := before
		baseline.Sources = nil
		baseline.Blocks = append([]Block(nil), before.Blocks...)
		for i := range baseline.Blocks {
			baseline.Blocks[i].Sources = nil
		}
		oldSchedule, newSchedule := Schedule(baseline, from, from.Add(48*time.Hour)), Schedule(after, from, from.Add(48*time.Hour))
		for i := range oldSchedule {
			oldSchedule[i].BlockID = ""
			oldSchedule[i].SourceID = ""
			oldSchedule[i].Rule = ""
			oldSchedule[i].Item.SourceID = ""
		}
		for i := range newSchedule {
			newSchedule[i].BlockID = ""
			newSchedule[i].SourceID = ""
			newSchedule[i].Rule = ""
			newSchedule[i].Item.SourceID = ""
		}
		if !reflect.DeepEqual(oldSchedule, newSchedule) {
			t.Fatalf("migration changed %s", before.ID)
		}
	}
}
