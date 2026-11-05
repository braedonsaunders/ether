package livetv

import (
	"github.com/braedonsaunders/ether/internal/core"
	"reflect"
	"testing"
	"time"
)

func TestProgrammingCalendar(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	episode := func(key int64) Item {
		return Item{Key: core.RatingKey(key), Title: "Sitcom", ShowKey: 99, Season: 1, Episode: int(key), DurationMS: 30 * 60 * 1000}
	}
	t.Run("pins preempt blocks and specials preempt pins", func(t *testing.T) {
		c := Channel{ID: "tv", Kind: "virtual", Mode: "order", Epoch: day, OffAir: true, Blocks: []Block{{ID: "prime", Start: 1200, End: 1380, Items: []Item{episode(1)}}}, Pins: []PinnedSlot{{ID: "strip", Name: "Weekday sitcom", Start: 1200, End: 1230, Days: []int{1, 2, 3, 4, 5}, Items: []Item{episode(2)}}, {ID: "premiere", Date: "2026-11-02", Start: 1200, End: 1260, Items: []Item{episode(3)}}}}
		ps := Schedule(c, day.Add(20*time.Hour), day.Add(23*time.Hour))
		if len(ps) == 0 || ps[0].Item.Key != 3 || !ps[0].Pinned {
			t.Fatalf("special did not win: %+v", ps)
		}
		for i := 1; i < len(ps); i++ {
			if ps[i].Start.Before(ps[i-1].End) {
				t.Fatal("overlap")
			}
		}
	})
	t.Run("strip continues across weekdays and skips weekend", func(t *testing.T) {
		c := Channel{ID: "strip", Kind: "virtual", Epoch: day, Mode: "order", OffAir: true, Pins: []PinnedSlot{{ID: "sitcom", Name: "Sitcom", Start: 1200, End: 1230, Days: []int{1, 2, 3, 4, 5}, Sources: []ProgramSource{{ID: "show", Kind: "show", Strip: true, Items: []Item{episode(1), episode(2), episode(3), episode(4), episode(5), episode(6)}}}}}}
		for offset, want := range map[int]int64{0: 1, 1: 2, 4: 5, 7: 6} {
			at := day.AddDate(0, 0, offset).Add(20 * time.Hour)
			ps := Schedule(c, at, at.Add(30*time.Minute))
			if len(ps) != 1 || int64(ps[0].Item.Key) != want {
				t.Fatalf("day %d want %d got %+v", offset, want, ps)
			}
		}
	})
	t.Run("weighted mix deterministic with arbitrary source kinds", func(t *testing.T) {
		sources := []ProgramSource{{ID: "a", Kind: "show", Weight: 3, Items: []Item{episode(1)}}, {ID: "b", Kind: "movies", Weight: 1, Items: []Item{{Key: 9, Title: "Movie", DurationMS: 3600000}}}}
		c := Channel{Seed: 35, Epoch: day}
		a := programmingItems(sources, nil, c, day, Block{ID: "mix"})
		b := programmingItems(sources, nil, c, day, Block{ID: "mix"})
		if !reflect.DeepEqual(a, b) {
			t.Fatal("nondeterministic")
		}
		counts := map[string]int{}
		for _, it := range a {
			counts[it.SourceID]++
		}
		if counts["a"] != 3 || counts["b"] != 1 {
			t.Fatalf("weights ignored: %v", counts)
		}
	})
	t.Run("repeat window survives day boundaries", func(t *testing.T) {
		c := Channel{ID: "repeat", Kind: "virtual", Epoch: day, Mode: "order", OffAir: true, Blocks: []Block{{ID: "daily", Start: 1200, End: 1230, Mode: "order", RepeatDays: 2, Items: []Item{episode(1), episode(2), episode(3)}}}}
		ps := Schedule(c, day, day.AddDate(0, 0, 5))
		last := map[int64]time.Time{}
		for _, p := range ps {
			if p.Item.Key == 0 {
				continue
			}
			key := int64(p.Item.Key)
			if at, ok := last[key]; ok && p.Start.Sub(at) < 48*time.Hour {
				t.Fatalf("repeated inside window: %+v", p)
			}
			last[key] = p.Start
		}
	})
	t.Run("overrun cut pad and shift", func(t *testing.T) {
		for _, fit := range []string{"cut", "pad", "shift"} {
			c := Channel{ID: "fit", Kind: "virtual", Epoch: day, Mode: "order", OffAir: true, Blocks: []Block{{ID: "movie", Start: 1200, End: 1260, Fit: fit, Sources: []ProgramSource{{ID: "movies", Kind: "items", Items: []Item{{Key: 1, Title: "Movie", DurationMS: 90 * 60 * 1000}}}}}, {ID: "after", Start: 1260, End: 1380, Items: []Item{episode(2)}}}}
			ps := Schedule(c, day.Add(20*time.Hour), day.Add(23*time.Hour))
			if fit == "pad" {
				if ps[0].Item.Key != 0 {
					t.Fatal("pad cut programme instead of filling")
				}
				continue
			}
			minutes := 60
			if fit == "shift" {
				minutes = 90
			}
			if ps[0].End.Sub(ps[0].Start) != time.Duration(minutes)*time.Minute {
				t.Fatalf("%s wrong overrun %+v", fit, ps[0])
			}
			for i := 1; i < len(ps); i++ {
				if ps[i].Start.Before(ps[i-1].End) {
					t.Fatalf("%s overlaps", fit)
				}
			}
		}
	})
}
