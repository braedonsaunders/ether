package livetv

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"reflect"
	"testing"
	"time"
)

type personalTestPicker struct{}

func (personalTestPicker) Picks(_ context.Context, u *core.User, _ map[string]float64, _ int) ([]core.PersonalPick, error) {
	return []core.PersonalPick{{Title: &core.Title{Key: core.MovieKey(u.ID), Type: core.TypeMovie, Title: u.Title + " pick", Runtime: time.Hour}, Kind: "picked", Reason: u.Title + " reason", Score: 1}}, nil
}

func TestPersonalRecomposeFrozenWindow(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	c := testChannel()
	c.Personal = &PersonalRecipe{Ingredients: []PersonalIngredient{{Kind: "picked", Share: 100, Enabled: true}}}
	var old []Programme
	for i := -2; i < 8; i++ {
		at := now.Add(time.Duration(i) * 20 * time.Minute)
		old = append(old, Programme{ID: fmt.Sprintf("%s-%d", c.ID, at.UnixMilli()), ChannelID: c.ID, Start: at, End: at.Add(20 * time.Minute), Item: Item{Key: core.MovieKey(int64(i + 10)), DurationMS: 1200000}, Reason: "Original"})
	}
	replacement := core.Title{Key: core.MovieKey(100), Type: core.TypeMovie, Runtime: 40 * time.Minute}
	got := composePersonal(c, old, []core.PersonalPick{{Title: &replacement, Kind: "picked", Reason: "Fresh", Score: 1}}, now)
	for i, p := range old {
		if !p.Start.After(now.Add(30*time.Minute)) && !reflect.DeepEqual(p, got[i]) {
			t.Fatalf("frozen slot changed: %v => %v", p, got[i])
		}
	}
	if got[4].Item.Key != replacement.Key {
		t.Fatal("mutable future was not recomposed")
	}
}

func TestPersonalViewersIsolated(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s := testService(t, Options{Picker: personalTestPicker{}, Now: func() time.Time { return now }})
	ctx := context.Background()
	c := testChannel()
	c.Items = nil
	c.Personal = &PersonalRecipe{NameFormat: "{viewer} TV", Ingredients: []PersonalIngredient{{Kind: "picked", Share: 100, Enabled: true}}}
	if _, err := s.SaveChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	viewers := []core.User{{ID: 1, Title: "Alice"}, {ID: 2, Title: "Bob"}}
	for _, u := range viewers {
		if err := s.buildPersonal(ctx, c, &u); err != nil {
			t.Fatal(err)
		}
	}

	for i := range viewers {
		u := &viewers[i]
		cs, err := s.AllowedChannels(ctx, u)
		if err != nil || len(cs) != 1 || cs[0].Name != u.Title+" TV" {
			t.Fatalf("lineup for %s: %#v %v", u.Title, cs, err)
		}
		ps, err := s.Guide(ctx, u, now, now.Add(time.Hour))
		if err != nil || len(ps) != 1 || ps[0].Item.Title != u.Title+" pick" || ps[0].Reason != u.Title+" reason" {
			t.Fatalf("rundown leak for %s: %#v %v", u.Title, ps, err)
		}
	}
	disabled := false
	if err := s.SavePolicy(ctx, Policy{UserID: 2, Personal: &PersonalPolicy{Enabled: &disabled}}); err != nil {
		t.Fatal(err)
	}
	if cs, err := s.AllowedChannels(ctx, &viewers[1]); err != nil || len(cs) != 0 {
		t.Fatal("disabled personal channel visible", cs, err)
	}
}

func TestPersonalCursorOnlyAdvancesDuringSessions(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s := testService(t, Options{Picker: personalTestPicker{}, Now: func() time.Time { return now }})
	c := testChannel()
	c.Personal = &PersonalRecipe{PauseWhenAway: true}
	u := &core.User{ID: 7, Title: "Alice"}
	p := Programme{ID: "p", Start: now, End: now.Add(time.Hour), Item: Item{Key: core.MovieKey(1), Title: "Resume", DurationMS: 3600000}}
	b, _ := json.Marshal([]Programme{p})
	if _, err := s.o.DB.Exec(`INSERT INTO livetv_personal(viewer_id,channel_id,day,rundown,cursor_ms,built_at) VALUES(?,?,?,?,?,?)`, u.ID, c.ID, "current", b, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillPersonalIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	c = s.personalChannel(context.Background(), c, u)
	s.beginPersonal(c, u)
	start := now
	now = now.Add(5 * time.Minute)
	s.endPersonal(c, u.ID)
	row, err := s.readPersonal(context.Background(), personalKey{u.ID, c.ID})
	if err != nil || row.cursor != start.Add(5*time.Minute).UnixMilli() {
		t.Fatal("watched time not persisted", row.cursor, err)
	}
	now = now.Add(time.Hour)
	ps, err := s.personalProgrammes(context.Background(), c, u, now, now.Add(time.Hour))
	if err != nil || len(ps) != 1 || ps[0].Offset(now) != 5*time.Minute {
		t.Fatal("away time advanced rundown", ps, err)
	}
	s.beginPersonal(c, u)
	now = now.Add(2 * time.Minute)
	s.endPersonal(c, u.ID)
	row, err = s.readPersonal(context.Background(), personalKey{u.ID, c.ID})
	if err != nil || row.cursor != start.Add(7*time.Minute).UnixMilli() {
		t.Fatal("resume time wrong", row.cursor, err)
	}
}
