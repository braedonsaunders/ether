package livetv

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// The materialized representations must preserve scheduling and policy while
// request cost depends only on the requested time slice, never the recipe.
func TestGuideSnapshotEquivalenceAndCost(t *testing.T) {
	for _, mode := range []string{"order", "marathon", "personal"} {
		for _, size := range []int{10, 500} {
			t.Run(fmt.Sprintf("%s/%d", mode, size), func(t *testing.T) {
				ctx := context.Background()
				now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
				s := testService(t, Options{Now: func() time.Time { return now }, Picker: costPersonalPicker{size}})
				s.o.DB.Exec(`CREATE TABLE resolver_availability (rating_key INTEGER PRIMARY KEY)`)
				s.o.Availability = sqlAvailability{s}
				c := testChannel()
				c.Items = nil
				c.Mode = mode
				for n := 0; n < size; n++ {
					c.Items = append(c.Items, Item{Key: core.MovieKey(int64(n + 1)), Title: fmt.Sprint(n), DurationMS: 3600000, Rating: "G"})
				}
				u := &core.User{ID: 1, IsOwner: true}
				if mode == "personal" {
					c.Mode = ""
					c.Personal = &PersonalRecipe{}
					c.Items = nil
				}
				if _, err := s.SaveChannel(ctx, c); err != nil {
					t.Fatal(err)
				}
				if mode == "personal" {
					if err := s.buildPersonal(ctx, c, u); err != nil {
						t.Fatal(err)
					}
				}
				until := now.Add(2 * time.Hour)
				for _, view := range []SnapshotView{SnapshotRaw, SnapshotPlayback, SnapshotGuide} {
					var want []Programme
					switch view {
					case SnapshotRaw:
						want = Schedule(c, now, until)
					case SnapshotPlayback:
						want = Schedule(s.guideChannel(ctx, c), now, until)
					case SnapshotGuide:
						want = s.broadcastGuide(s.guideChannel(ctx, c), now, until)
					}
					if mode == "personal" {
						row, err := s.readPersonal(ctx, personalKey{u.ID, c.ID})
						if err != nil {
							t.Fatal(err)
						}
						want = programmeSlice(row.programmes, now, until)
					}
					got, err := s.ProgrammesSnapshot(ctx, u, c.ID, now, until, view)
					if err != nil {
						t.Fatal(err)
					}
					a, _ := json.Marshal(got)
					b, _ := json.Marshal(want)
					if string(a) != string(b) {
						t.Fatalf("%s snapshot changed programme bytes\ngot %s\nwant %s", view, a, b)
					}
				}
				measuredBefore, beforeStats := store.WithQueryStats(ctx)
				beforeStart := time.Now()
				beforeCtx, _, _ := s.withPolicy(measuredBefore, u)
				beforeChannel, _ := s.Channel(beforeCtx, c.ID)
				if mode == "personal" {
					_, _ = s.readPersonal(beforeCtx, personalKey{u.ID, c.ID})
				} else {
					for _, p := range s.broadcastGuide(s.guideChannel(beforeCtx, beforeChannel), now, until) {
						_ = s.allowed(beforeCtx, u, beforeChannel, &p)
					}
				}
				t.Logf("legacy guide titles=%d queries=%d elapsed=%s", size, beforeStats.Queries(), time.Since(beforeStart))
				for _, kind := range []string{"guide", "on now"} {
					measured, stats := store.WithQueryStats(ctx)
					start := time.Now()
					if kind == "guide" {
						ps, err := s.GuideForChannels(measured, u, now, until, []string{c.ID})
						if err != nil || len(ps) == 0 {
							t.Fatalf("guide %d %v", len(ps), err)
						}
					} else {
						_, ps, err := s.OnNow(measured, u, now, until)
						if err != nil || len(ps) == 0 {
							t.Fatalf("on now %d %v", len(ps), err)
						}
					}
					if stats.Queries() > 5 {
						t.Fatalf("%s: %d queries", kind, stats.Queries())
					}
					for _, statement := range stats.Statements() {
						if strings.Contains(statement.SQL, "resolver_availability") {
							t.Fatal("availability queried by viewer")
						}
					}
					t.Logf("%s titles=%d queries=%d elapsed=%s", kind, size, stats.Queries(), time.Since(start))
				}
				// A channel save publishes its replacement before returning.
				if mode != "personal" {
					c.Items[0].Title = "Edited"
					if _, err := s.SaveChannel(ctx, c); err != nil {
						t.Fatal(err)
					}
					got, err := s.ProgrammesSnapshot(ctx, u, c.ID, now, until, SnapshotRaw)
					if err != nil {
						t.Fatal(err)
					}
					want := Schedule(c, now, until)
					for i := range got {
						got[i].policyTag = nil
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatal("owner save left stale snapshot")
					}
				}
			})
		}
	}
}

type costPersonalPicker struct{ size int }

func (p costPersonalPicker) Picks(context.Context, *core.User, map[string]float64, int) ([]core.PersonalPick, error) {
	out := make([]core.PersonalPick, p.size)
	for i := range out {
		out[i] = core.PersonalPick{Title: &core.Title{Key: core.MovieKey(int64(i + 1)), Type: core.TypeMovie, Title: fmt.Sprint(i), Runtime: 5 * time.Minute, ContentRating: "G"}, Kind: "picked", Score: 1}
	}
	return out, nil
}
