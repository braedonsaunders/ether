package livetv

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestLegacyJSONKeepsTheSameSchedule(t *testing.T) {
	raw := []byte(`{"id":"test","number":"1","name":"Owner channel","kind":"virtual","enabled":true,"epoch":"1970-01-01T00:00:00Z","shuffle":true,"seed":71,"items":[{"key":1,"title":"First","durationMs":12000,"rating":"G"},{"key":2,"title":"Second","durationMs":20000,"rating":"PG"}]}`)
	var c Channel
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	from := time.Unix(10_000, 0).UTC()
	got := Schedule(c, from, from.Add(time.Hour))
	if len(got) == 0 {
		t.Fatal("no schedule")
	}
	p := got[len(got)/2]
	again := Schedule(c, p.Start.Add(time.Millisecond), p.End.Add(-time.Millisecond))
	if len(again) != 1 || !reflect.DeepEqual(again[0], p) {
		t.Fatalf("legacy window drifted")
	}
	// A zero-value weekly field must not leave the original engine.
	if !legacyCalendar(c) {
		t.Fatal("old channel was treated as a weekly grid")
	}
}

func programmeAt(ps []Programme, start time.Time) Programme {
	for _, p := range ps {
		if p.Start.Equal(start) {
			return p
		}
	}
	return Programme{}
}

func TestWeekdayBlockStaysOffOtherDays(t *testing.T) {
	// Saturday 8:00–12:00 in New York. 31 Oct 2026 is a Saturday.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	c := testChannel()
	c.Timezone = ny.String()
	c.Blocks = []Block{{
		Start: 8 * 60,
		End:   12 * 60,
		Days:  []int{int(time.Saturday)},
		Items: []Item{{Key: core.MovieKey(9), Title: "Toons", DurationMS: int64(time.Hour / time.Millisecond)}},
	}}
	sat := time.Date(2026, 10, 31, 0, 0, 0, 0, ny)
	ps := Schedule(c, sat, sat.Add(24*time.Hour))
	var toons int
	for _, p := range ps {
		if p.Item.Title == "Toons" {
			toons++
			if p.Start.In(ny).Weekday() != time.Saturday || p.Start.In(ny).Hour() < 8 || !p.Start.Before(sat.Add(12*time.Hour)) {
				t.Fatalf("cartoon outside Saturday morning: %s", p.Start.In(ny))
			}
		}
	}
	if toons == 0 {
		t.Fatal("Saturday block did not air")
	}
	fri := sat.AddDate(0, 0, -1)
	for _, p := range Schedule(c, fri, fri.Add(24*time.Hour)) {
		if p.Item.Title == "Toons" {
			t.Fatalf("Saturday block aired on Friday: %s", p.Start)
		}
	}
}

func TestShuffleKeepsEpisodesInOrder(t *testing.T) {
	show := core.ShowKey(42)
	c := testChannel()
	c.Mode = "shuffle"
	c.Seed = 3
	c.Items = []Item{
		{Key: core.EpisodeKey(42, 1, 2), Title: "Ep2", DurationMS: int64(time.Hour / time.Millisecond), ShowKey: show, Season: 1, Episode: 2},
		{Key: core.MovieKey(7), Title: "Movie", DurationMS: int64(time.Hour / time.Millisecond)},
		{Key: core.EpisodeKey(42, 1, 1), Title: "Ep1", DurationMS: int64(time.Hour / time.Millisecond), ShowKey: show, Season: 1, Episode: 1},
	}
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	ps := Schedule(c, day, day.Add(3*time.Hour))
	if len(ps) < 3 {
		t.Fatalf("short: %+v", ps)
	}
	var ep1, ep2 int = -1, -1
	for i, p := range ps {
		if p.Item.Title == "Ep1" && ep1 < 0 {
			ep1 = i
		}
		if p.Item.Title == "Ep2" && ep2 < 0 {
			ep2 = i
		}
	}
	if ep1 < 0 || ep2 < 0 || ep1 > ep2 {
		t.Fatalf("episodes out of order: %+v", titles(ps))
	}
}

func TestAlignCutStartsOnTheHour(t *testing.T) {
	c := testChannel()
	c.Align = "hour"
	c.Fit = "cut"
	c.Items = []Item{{Key: core.MovieKey(1), Title: "Long", DurationMS: int64(90 * time.Minute / time.Millisecond)}}
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	ps := Schedule(c, day, day.Add(3*time.Hour))
	if len(ps) < 3 {
		t.Fatalf("short: %+v", ps)
	}
	for i, p := range ps {
		if p.Start.Minute() != 0 || p.Start.Second() != 0 {
			t.Fatalf("programme %d starts at %s", i, p.Start)
		}
		if p.End.Sub(p.Start) != time.Hour {
			t.Fatalf("cut fit kept %s", p.End.Sub(p.Start))
		}
		if i > 0 && !p.Start.Equal(ps[i-1].End) {
			t.Fatal("gap")
		}
	}
	narrow := Schedule(c, ps[1].Start.Add(time.Minute), ps[1].Start.Add(2*time.Minute))
	if len(narrow) != 1 || !narrow[0].Start.Equal(ps[1].Start) || narrow[0].Item.Title != ps[1].Item.Title {
		t.Fatalf("window changed the hour: %+v vs %+v", narrow, ps[1])
	}
}

func TestAlignPadUsesFillerThenTheNextHour(t *testing.T) {
	c := testChannel()
	c.Align = "hour"
	c.Fit = "pad"
	c.Items = []Item{{Key: core.MovieKey(1), Title: "Short", DurationMS: int64(20 * time.Minute / time.Millisecond)}}
	c.Filler = []Item{{Key: core.MovieKey(2), Title: "Bumper", DurationMS: int64(5 * time.Minute / time.Millisecond)}}
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	ps := Schedule(c, day, day.Add(time.Hour))
	if len(ps) < 2 || ps[0].Item.Title != "Short" || ps[0].End.Sub(ps[0].Start) != 20*time.Minute {
		t.Fatalf("feature: %+v", ps)
	}
	if ps[1].Item.Title != "Bumper" {
		t.Fatalf("want bumper, got %+v", ps[1])
	}
	last := ps[len(ps)-1]
	if !last.End.Equal(day.Add(time.Hour)) {
		t.Fatalf("hour not filled: %s", last.End)
	}
	for i := 1; i < len(ps); i++ {
		if !ps[i].Start.Equal(ps[i-1].End) {
			t.Fatal("gap")
		}
	}
}

func TestRepeatSkipsAndStaysWindowStable(t *testing.T) {
	c := testChannel()
	c.Mode = "order"
	c.RepeatDays = 1
	c.OffAir = true
	c.Items = []Item{
		{Key: core.MovieKey(1), Title: "A", DurationMS: int64(time.Hour / time.Millisecond)},
		{Key: core.MovieKey(2), Title: "B", DurationMS: int64(time.Hour / time.Millisecond)},
	}
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	ps := Schedule(c, day, day.Add(6*time.Hour))
	seen := map[string]time.Time{}
	for _, p := range ps {
		if p.Item.Slate == offAirSlate {
			continue
		}
		if prev, ok := seen[p.Item.Title]; ok && p.Start.Sub(prev) < 24*time.Hour {
			t.Fatalf("%s repeated inside a day: %s then %s", p.Item.Title, prev, p.Start)
		}
		seen[p.Item.Title] = p.Start
	}
	if seen["A"].IsZero() || seen["B"].IsZero() {
		t.Fatalf("missing titles: %+v", titles(ps))
	}
	one := Schedule(c, day.Add(90*time.Minute), day.Add(90*time.Minute+time.Minute))
	wide := programmeAt(ps, one[0].Start)
	if len(one) != 1 || !reflect.DeepEqual(one[0], wide) {
		t.Fatalf("repeat window drifted: %+v / %+v", one, wide)
	}
}

func TestOffAirFillsADryChannel(t *testing.T) {
	c := testChannel()
	c.Items = nil
	c.OffAir = true
	day := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	ps := Schedule(c, day, day.Add(time.Hour))
	if len(ps) != 1 || ps[0].Item.Slate != offAirSlate || !ps[0].Start.Before(day) && !ps[0].Start.Equal(day) || !ps[0].End.After(day) {
		t.Fatalf("slate: %+v", ps)
	}
	if !ps[0].Start.Before(ps[0].End) || ps[0].Offset(day) != day.Sub(ps[0].Start) {
		t.Fatal("offset")
	}
}

func TestNewestPlaysTheNewerShowFirstAndKeepsEpisodesOrdered(t *testing.T) {
	c := testChannel()
	c.Mode = "newest"
	old := core.ShowKey(1)
	newer := core.ShowKey(2)
	c.Items = []Item{
		{Key: core.EpisodeKey(1, 1, 1), Title: "Old1", DurationMS: int64(time.Hour / time.Millisecond), ShowKey: old, Season: 1, Episode: 1, ReleasedMS: 100},
		{Key: core.EpisodeKey(2, 1, 2), Title: "New2", DurationMS: int64(time.Hour / time.Millisecond), ShowKey: newer, Season: 1, Episode: 2, ReleasedMS: 300},
		{Key: core.EpisodeKey(2, 1, 1), Title: "New1", DurationMS: int64(time.Hour / time.Millisecond), ShowKey: newer, Season: 1, Episode: 1, ReleasedMS: 200},
	}
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	ps := Schedule(c, day, day.Add(3*time.Hour))
	if titles(ps)[:2][0] != "New1" || titles(ps)[1] != "New2" {
		t.Fatalf("order: %v", titles(ps))
	}
}

func TestBlocksOnDifferentDaysMayShareAClockTime(t *testing.T) {
	c := testChannel()
	c.Blocks = []Block{
		{Start: 60, End: 120, Days: []int{1}, Items: []Item{{Key: core.MovieKey(3), Title: "Mon", DurationMS: int64(time.Hour / time.Millisecond)}}},
		{Start: 60, End: 120, Days: []int{5}, Items: []Item{{Key: core.MovieKey(4), Title: "Fri", DurationMS: int64(time.Hour / time.Millisecond)}}},
	}
	if err := validateChannel(&c); err != nil {
		t.Fatal(err)
	}
	c.Blocks[1].Days = []int{1}
	if validateChannel(&c) == nil {
		t.Fatal("same-day overlap accepted")
	}
}

func titles(ps []Programme) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Item.Title
	}
	return out
}
