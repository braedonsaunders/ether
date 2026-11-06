package livetv

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"io"
	"reflect"
	"testing"
	"time"
)

func goldenChannel() Channel {
	return Channel{ID: "gold", Kind: "virtual", Mode: "shuffle", Seed: 42, Timezone: "America/Toronto", Align: "half", Items: []Item{{Key: 1, Title: "One", DurationMS: 1250000}, {Key: 2, Title: "Two", DurationMS: 2500000}}, Blocks: []Block{{Start: 600, End: 900, Days: []int{1, 3, 5}, Items: []Item{{Key: 3, Title: "Three", DurationMS: 1100000}}}}}
}
func TestNoLookWeekGolden(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	b, _ := json.Marshal(Schedule(goldenChannel(), from, from.AddDate(0, 0, 7)))
	got := fmt.Sprintf("%x", sha256.Sum256(b))
	t.Log(got)
	if got != "732264dddf663f4019748958389d04ef1be478b581e737b48874fd9ca901ea6d" {
		t.Fatal(got)
	}
}

type testGraphics struct{ cached bool }

func (g testGraphics) Duration(string, string) (time.Duration, bool) { return 8 * time.Second, true }
func (g testGraphics) Clip(context.Context, core.ClipRequest) (core.ClipRef, bool) {
	return core.ClipRef{Hash: "test", Duration: 8 * time.Second, Path: "/cache/ident.mp4"}, g.cached
}
func (testGraphics) Ensure(context.Context, core.ClipRequest) {}

func TestInterstitialSchedule(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, placement := range []string{"gap", "insert"} {
		t.Run(placement, func(t *testing.T) {
			c := goldenChannel()
			if placement == "insert" {
				c.Align = ""
			}
			plain := Schedule(c, from, from.AddDate(0, 0, 2))
			c.Look = &ChannelLook{LookID: "look", UpNext: true, Breaks: "between", Placement: placement}
			c.clipDuration = testGraphics{}.Duration
			all := Schedule(c, from, from.AddDate(0, 0, 2))
			narrow := Schedule(c, from.Add(7*time.Hour+13*time.Second), from.Add(30*time.Hour))
			var overlap []Programme
			for _, p := range all {
				if p.End.After(from.Add(7*time.Hour+13*time.Second)) && p.Start.Before(from.Add(30*time.Hour)) {
					overlap = append(overlap, p)
				}
			}
			if !reflect.DeepEqual(overlap, narrow) {
				t.Fatal("overlapping windows differ")
			}
			clips := 0
			for _, p := range all {
				if interstitial(p.Item) {
					clips++
				}
			}
			if clips == 0 {
				t.Fatal("no clips")
			}
			if placement == "gap" {
				if !reflect.DeepEqual(guideSchedule(c, from, from.AddDate(0, 0, 2)), plain) {
					t.Fatal("gap guide changed")
				}
				ids := map[string]bool{}
				for _, p := range plain {
					if p.Item.Key != 0 {
						ids[p.ID] = true
					}
				}
				for _, p := range all {
					if p.Item.Key != 0 && !ids[p.ID] {
						t.Fatal("real programme ID moved", p.ID)
					}
				}
			}
		})
	}
}

func TestGapLookKeepsEpochCalendar(t *testing.T) {
	c := testChannel()
	from := time.Unix(253001, 0).UTC()
	plain := Schedule(c, from, from.Add(7*24*time.Hour))
	c.Look = &ChannelLook{LookID: "look", UpNext: true}
	c.clipDuration = testGraphics{}.Duration
	if !reflect.DeepEqual(plain, Schedule(c, from, from.Add(7*24*time.Hour))) {
		t.Fatal("gap look changed the epoch calendar")
	}
}

func TestMissingBroadcastClipPreservesScheduleAndGuide(t *testing.T) {
	for _, placement := range []string{"gap", "insert"} {
		t.Run(placement, func(t *testing.T) {
			now := time.Date(2026, 11, 2, 0, 0, 25, 0, time.UTC)
			s := testService(t, Options{Graphics: testGraphics{}, Now: func() time.Time { return now }})
			c := Channel{ID: "missing", Kind: "virtual", Mode: "order", Items: []Item{{Key: 1, Title: "Programme", DurationMS: 20000}}, Look: &ChannelLook{LookID: "look", UpNext: true, Placement: placement}}
			if placement == "gap" {
				c.Align = "half"
			}
			c = s.graphicsChannel(c)
			until := now.Add(24 * time.Second)
			scheduled := Schedule(c, now, until)
			guideBefore := s.broadcastGuide(c, now, until)
			aired := s.broadcastSchedule(context.Background(), c, now, until, true)
			if len(aired) != len(scheduled) {
				t.Fatal("missing master changed the rundown")
			}
			fallbacks := 0
			for i, p := range aired {
				want := scheduled[i]
				if p.ID != want.ID || p.Start != want.Start || p.End != want.End || !reflect.DeepEqual(p.Item, want.Item) {
					t.Fatalf("airing %d shifted: %v, want %v", i, p, want)
				}
				if interstitial(p.Item) && !p.Start.After(now) {
					// An already-airing clip ending early takes the identical
					// failure path as the CHROMA pod EOF incident. A refresh
					// must preserve every later prepared programme identity.
					s.recordGraphicsSkip(c, p, io.EOF)
					refreshed := s.broadcastSchedule(context.Background(), c, now, until, true)
					for j, row := range refreshed {
						if row.ID != scheduled[j].ID || row.Start != scheduled[j].Start || row.End != scheduled[j].End || broadcastProgrammeID(row) != broadcastProgrammeID(scheduled[j]) {
							t.Fatal("EOF recompute invalidated prepared input")
						}
					}
					if !p.graphicsFallback {
						t.Fatal("missing master did not latch fallback")
					}
					fallbacks++
				}
			}
			if fallbacks == 0 {
				t.Fatal("fixture did not cover a missing moment")
			}
			if !reflect.DeepEqual(guideBefore, s.broadcastGuide(c, now, until)) {
				t.Fatal("fallback changed the guide")
			}
		})
	}
}
