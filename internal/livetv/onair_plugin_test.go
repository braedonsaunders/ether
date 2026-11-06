package livetv

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
	"reflect"
	"testing"
	"time"
)

type pluginGraphics struct{ fresh bool }

func (g *pluginGraphics) Duration(string, string) (time.Duration, bool) {
	return 120 * time.Second, true
}
func (g *pluginGraphics) Clip(context.Context, core.ClipRequest) (core.ClipRef, bool) {
	return core.ClipRef{}, g.fresh
}
func (g *pluginGraphics) Ensure(context.Context, core.ClipRequest)  {}
func (g *pluginGraphics) SegmentInfo(string) (string, string, bool) { return "Forecast", "", true }
func TestPluginClockStalePreservesSchedule(t *testing.T) {
	g := &pluginGraphics{fresh: true}
	s := &Service{o: Options{Graphics: g, Now: time.Now}}
	c := s.graphicsChannel(Channel{ID: "clock", Kind: "virtual", Clock: []ClockSlot{{SegmentID: "plugin:weather/forecast", Minute: 58, EveryMinutes: 60, From: 360, Until: 1380}}, OffAir: true})
	from := time.Date(2026, 11, 5, 6, 0, 0, 0, time.UTC)
	until := from.Add(3 * time.Hour)
	good := Schedule(c, from, until)
	g.fresh = false
	stale := Schedule(c, from, until)
	if !reflect.DeepEqual(good, stale) {
		t.Fatal("feed freshness changed pure schedule")
	}
	n := 0
	for _, p := range good {
		if p.Item.Slate == "plugin:weather/forecast" {
			n++
			if p.Start.Minute() != 58 || p.End.Sub(p.Start) != 120*time.Second {
				t.Fatal(p)
			}
		}
	}
	if n != 3 {
		t.Fatalf("got %d slots", n)
	}
	overlap := Schedule(c, from.Add(71*time.Minute), until)
	var want []Programme
	for _, p := range good {
		if p.End.After(from.Add(71 * time.Minute)) {
			want = append(want, p)
		}
	}
	if !reflect.DeepEqual(want, overlap) {
		t.Fatal("window-dependent clock")
	}
	// Missing/stale clip occupies the same interval using plain fallback.
	s.o.Logger = nil
	s.graphicsSkips = map[string]time.Time{}
	for _, p := range good {
		if interstitial(p.Item) {
			s.graphicsSkips[c.ID+"/"+p.ID] = p.End
		}
	}
	fallback := s.broadcastSchedule(context.Background(), c, from, until, false)
	if len(fallback) != len(good) {
		t.Fatal("fallback changed schedule rows")
	}
	for i, p := range fallback {
		want := good[i]
		if p.ID != want.ID || p.Start != want.Start || p.End != want.End {
			t.Fatal("fallback shifted a clock boundary")
		}
		if interstitial(p.Item) && !p.graphicsFallback {
			t.Fatal("stale segment did not retain its fallback interval")
		}
	}

}
func TestPluginUnratedChannelRule(t *testing.T) {
	c := Channel{Rating: "PG", Look: &ChannelLook{}, clipDuration: (&pluginGraphics{}).Duration}
	if _, ok := c.pluginItem("plugin:weather/forecast"); ok {
		t.Fatal("unrated segment on capped channel")
	}
	c.Look.AllowUnratedSegments = true
	if _, ok := c.pluginItem("plugin:weather/forecast"); !ok {
		t.Fatal("explicit allowance ignored")
	}
	c.segmentInfo = func(string) (string, string, bool) { return "News", "R", true }
	if _, ok := c.pluginItem("plugin:weather/forecast"); ok {
		t.Fatal("rating cap bypassed")
	}
	c.Rating = ""
	c.Filter = &core.Filter{MaxContentRating: "PG"}
	if _, ok := c.pluginItem("plugin:weather/forecast"); ok {
		t.Fatal("filter cap bypassed")
	}
}

func TestPluginViewerPolicy(t *testing.T) {
	s := testService(t, Options{})
	c := Channel{ID: "policy-plugin", Enabled: true, Look: &ChannelLook{AllowUnratedSegments: true}}
	p := Programme{Item: Item{Slate: "plugin:weather/forecast"}}
	u := &core.User{ID: 10, MaxContentRating: "PG"}
	if s.allowed(context.Background(), u, c, &p) {
		t.Fatal("unrated segment bypassed viewer cap")
	}
	p.Item.Rating = "R"
	if s.allowed(context.Background(), u, c, &p) {
		t.Fatal("declared segment rating bypassed viewer cap")
	}
	p.Item.Rating = "G"
	if !s.allowed(context.Background(), u, c, &p) {
		t.Fatal("rated segment below viewer cap denied")
	}
}

// Plugin moments are switcher inputs, including a clock-only channel with no look.
type pluginPlanGraphics struct{ pluginGraphics }

func (*pluginPlanGraphics) Clip(_ context.Context, r core.ClipRequest) (core.ClipRef, bool) {
	if r.Moment != "plugin:weather/forecast" {
		return core.ClipRef{}, false
	}
	return core.ClipRef{Hash: "forecast-feed-version", Duration: 120 * time.Second, Path: "/cache/forecast.mp4"}, true
}
func TestPluginClockBroadcastInputPlan(t *testing.T) {
	now := time.Date(2026, 11, 5, 6, 58, 10, 0, time.UTC)
	g := &pluginPlanGraphics{}
	s := testService(t, Options{Graphics: g, Now: func() time.Time { return now }})
	c := Channel{ID: "switcher-plugin", Kind: "virtual", Enabled: true, OffAir: true, Clock: []ClockSlot{{SegmentID: "plugin:weather/forecast", Minute: 58, EveryMinutes: 60, From: 360, Until: 1380}}}
	for _, look := range []*ChannelLook{nil, {LookID: "meridian"}} {
		c.Look = look
		var got transcode.DecoderRequest
		runner := &broadcastFakeRunner{decoder: func(_ context.Context, r transcode.DecoderRequest) (transcode.Decoder, error) {
			got = r
			return &broadcastFakeDecoder{closed: make(chan struct{})}, nil
		}}
		planner := &scheduleBroadcastPlanner{s: s, channel: c, user: core.User{ID: 1, IsOwner: true}, runner: runner, format: smallBroadcastFormat()}
		planner.startPlanner(t.Context())
		defer planner.closePlanner()
		plan, _ := planner.plans(now)
		deadline := time.Now().Add(time.Second)
		for plan.start.IsZero() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			plan, _ = planner.plans(now)
		}
		if !plan.start.Equal(now.Add(-10*time.Second)) || !plan.end.Equal(now.Add(110*time.Second)) {
			t.Fatalf("wrong clock input boundaries: %v %v", plan.start, plan.end)
		}
		decoder, release, err := plan.open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		decoder.Close()
		if release != nil {
			release()
		}
		if got.InputURL != "/cache/forecast.mp4" || got.Loop || got.SessionID != "graphics-forecast-feed-version" {
			t.Fatalf("plugin not passed to continuous decoder: %+v", got)
		}
	}
	personal := c
	personal.Personal = &PersonalRecipe{}
	personal.personalViewer = &core.User{ID: 42, Title: "Alex"}
	request := s.clipRequest(personal, Programme{Reason: "A personal pick", Item: Item{Slate: "plugin:weather/forecast"}})
	if request.ViewerID != 42 || request.Bind["viewer.name"] != "Alex" || request.Bind["next.reason"] != "A personal pick" {
		t.Fatalf("lost personal bindings: %+v", request)
	}
}
