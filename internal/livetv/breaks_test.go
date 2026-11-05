package livetv

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
)

func TestBreakAnalysisNeverOnRequestPath(t *testing.T) {
	s := testService(t, Options{})
	s.o.BreakInput = func(context.Context, core.RatingKey) (markers.BreakInput, error) {
		t.Fatal("request opened analysis source")
		return markers.BreakInput{}, nil
	}
	s.o.BreakAnalyzer = &markers.BreakAnalyzer{DB: s.o.DB}
	c := testChannel()
	p, _ := EraBreakPattern("1980s")
	c.Look = &ChannelLook{BreakPattern: &p, ReelID: "ads"}
	c.Items = []Item{{Key: core.EpisodeKey(1, 1, 1), ShowKey: core.ShowKey(1), DurationMS: 1320000, Title: "Episode"}}
	if _, err := s.SaveChannel(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Channel(t.Context(), c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Guide(t.Context(), nil, time.Unix(0, 0), time.Unix(1800, 0)); err != nil {
		t.Fatal(err)
	}
	// Publishing occurs only from the background planner; request reads contain
	// the points without any callback, probe or detector work.
	s.publishBreaks(c.Items[0].Key, []markers.BreakPoint{{AtMS: 420000, Source: "detected", Confidence: .96}, {AtMS: 900000, Source: "chapters", Confidence: .99}}, 1320000, "release", "revision")
	got, err := s.Channel(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items[0].BreakPoints) != 2 || got.Items[0].BreakReleaseID != "release" {
		t.Fatalf("missing saved planning snapshot: %+v", got.Items[0])
	}
}

func TestBreakPublishNestedSources(t *testing.T) {
	s := testService(t, Options{})
	raw := `{"id":"nested","kind":"virtual","look":{"breakPattern":{"inside":true,"podsPerHalfHour":2}},"blocks":[{"sources":[{"items":[{"key":123,"durationMs":1320000}]}]}],"futureField":"preserve"}`
	if _, err := s.o.DB.Exec(`INSERT INTO livetv_channels VALUES(?,?)`, "nested", raw); err != nil {
		t.Fatal(err)
	}
	s.publishBreaks(123, []markers.BreakPoint{{AtMS: 420000, Source: "detected", Confidence: .96}}, 1320000, "release", "revision")
	var saved string
	if err := s.o.DB.QueryRow(`SELECT data FROM livetv_channels WHERE id=?`, "nested").Scan(&saved); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(saved), &doc); err != nil {
		t.Fatal(err)
	}
	item := doc["blocks"].([]any)[0].(map[string]any)["sources"].([]any)[0].(map[string]any)["items"].([]any)[0].(map[string]any)
	if len(item["breakPoints"].([]any)) != 1 || doc["futureField"] != "preserve" {
		t.Fatal(saved)
	}
}

func TestNaturalBreakOverrideInheritance(t *testing.T) {
	var document any
	raw := `{"blocks":[{"breaks":{"breakPattern":{"inside":true,"podsPerHalfHour":2}},"sources":[{"items":[{"key":1,"durationMs":1320000}]}]},{"breaks":{},"items":[{"key":2,"durationMs":1320000}]},{"breaks":{"breakPlacement":"even","breakPattern":{"inside":true,"podsPerHalfHour":2}},"items":[{"key":3,"durationMs":1320000}]}]}`
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		t.Fatal(err)
	}
	var ids []float64
	walkNaturalBreakItems(document, nil, func(item map[string]any, _ *ChannelLook) { ids = append(ids, item["key"].(float64)) })
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatal(ids)
	}
}
