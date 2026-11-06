package livetv

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
)

func TestBreakAnalysisCapturesOnlyAdmittedIdentity(t *testing.T) {
	type requestKey struct{}
	s := testService(t, Options{InputUser: func(ctx context.Context) (*core.User, error) {
		if ctx.Value(requestKey{}) != true {
			return nil, core.ErrNotFound
		}
		return &core.User{ID: core.SetupOwnerID, IsOwner: true}, nil
	}})
	c := testChannel()
	p, _ := EraBreakPattern("1980s")
	c.Look = &ChannelLook{BreakPattern: &p, BreakPlacement: "natural", ReelID: "ads"}
	c.Items = []Item{{Key: core.EpisodeKey(1, 1, 1), ShowKey: core.ShowKey(1), DurationMS: 1320000, Title: "Episode"}}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), requestKey{}, true))
	if _, err := s.SaveChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	cancel()
	calls := 0
	s.o.BreakInput = func(ctx context.Context, _ core.RatingKey, id int64) (markers.BreakInput, error) {
		calls++
		if id != core.SetupOwnerID || ctx.Err() != nil || ctx.Value(requestKey{}) != nil {
			t.Fatalf("retained request instead of identity: id=%d err=%v value=%v", id, ctx.Err(), ctx.Value(requestKey{}))
		}
		return markers.BreakInput{}, errors.New("fixture stops before source open")
	}
	s.analyzeUpcomingBreaks(map[core.RatingKey]time.Time{})
	if calls != 1 {
		t.Fatalf("background analysis calls: %d", calls)
	}
}

func TestBreakAnalysisNeverOnRequestPath(t *testing.T) {
	s := testService(t, Options{Now: func() time.Time { return time.Unix(0, 0) }})
	s.o.BreakInput = func(context.Context, core.RatingKey, int64) (markers.BreakInput, error) {
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
	// W22 publishes guide snapshots in the background; warm that snapshot
	// before asserting that the request itself never opens an analysis source.
	if err := s.refreshVirtualGuide(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Guide(t.Context(), nil, time.Unix(0, 0), time.Unix(1800, 0)); err != nil {
		t.Fatal(err)
	}
	// Publishing occurs only from the background planner; request reads contain
	// the points without any callback, probe or detector work.
	s.publishBreaks(c.Items[0].Key, []markers.BreakPoint{{AtMS: 420000, Source: "detected", Confidence: .96}, {AtMS: 900000, Source: "chapters", Confidence: .99}}, 1320000, "release", "revision")
	if err := s.refreshVirtualGuide(t.Context()); err != nil {
		t.Fatal(err)
	}
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

func TestBreakSnapshotsApplyOnlyToLaterAirings(t *testing.T) {
	start := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	item := Item{Key: core.EpisodeKey(1, 1, 1), DurationMS: 1320000, BreakPointsMS: []int64{420000}, BreakPoints: []BreakPoint{{AtMS: 420000, Source: "detected", Confidence: .91}}, BreakReleaseID: "release", BreakRevision: "revision", BreakAvailableAfterMS: start.Add(10 * time.Minute).UnixMilli()}
	current := availableBreakItem(item, start)
	if len(current.BreakPointsMS) != 0 || current.BreakReleaseID != "" {
		t.Fatal("new evidence changed current airing", current)
	}
	later := availableBreakItem(item, start.Add(30*time.Minute))
	if len(later.BreakPointsMS) != 1 || later.BreakRevision != "revision" {
		t.Fatal("later airing lost cached evidence", later)
	}
	if len(item.BreakPointsMS) != 1 {
		t.Fatal("schedule mutated stored item")
	}
}
