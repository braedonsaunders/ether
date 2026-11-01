package livetv

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type scriptedPlayback struct {
	*fakePlayback
	fail  map[core.RatingKey]bool
	block core.RatingKey
	calls []core.RatingKey
}

func (g *scriptedPlayback) Prepare(ctx context.Context, u *core.User, k core.RatingKey, c core.ClientCaps) (*core.PlaySession, error) {
	g.calls = append(g.calls, k)
	if g.block != 0 && k == g.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if g.fail[k] {
		return nil, core.ErrNotAvailable
	}
	return g.fakePlayback.Prepare(ctx, u, k, c)
}

func (g *scriptedPlayback) PrepareChannel(ctx context.Context, u *core.User, k core.RatingKey, c core.ClientCaps, group string) (*core.PlaySession, error) {
	g.groups = append(g.groups, group)
	return g.Prepare(ctx, u, k, c)
}

func TestGuideSkipsTitlesKnownToBeUnplayable(t *testing.T) {
	now := time.Unix(7, 0).UTC()
	first := core.MovieKey(1)
	second := core.MovieKey(2)
	s := testService(t, Options{
		Now: func() time.Time { return now },
		Availability: availFunc(func(key core.RatingKey) (bool, bool) {
			if key == first {
				return true, false
			}
			return false, false
		}),
	})
	c := testChannel()
	c.Items = []Item{
		{Key: first, Title: "Cats in the Museum 2", DurationMS: 12000},
		{Key: second, Title: "Second", DurationMS: 20000},
	}
	if _, err := s.SaveChannel(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got, err := s.programmes(context.Background(), c, now, now.Add(time.Second))
	if err != nil || len(got) != 1 || got[0].Item.Key != second || got[0].Item.Title != "Second" {
		t.Fatalf("guide kept an unplayable title: %+v %v", got, err)
	}
	unknown := testService(t, Options{Now: func() time.Time { return now }})
	plain, err := unknown.programmes(context.Background(), c, now, now.Add(time.Second))
	if err != nil || len(plain) != 1 || plain[0].Item.Key != first {
		t.Fatalf("unknown availability changed the guide: %+v %v", plain, err)
	}
}

type availFunc func(core.RatingKey) (bool, bool)

func (f availFunc) Available(_ context.Context, key core.RatingKey) (bool, bool) { return f(key) }

func TestVirtualTuneSkipsToTheNextPlayableItem(t *testing.T) {
	now := time.Unix(7, 0).UTC()
	first, second := core.MovieKey(1), core.MovieKey(2)
	pb := &scriptedPlayback{fakePlayback: &fakePlayback{}, fail: map[core.RatingKey]bool{first: true}}
	s := testService(t, Options{Now: func() time.Time { return now }, Playback: pb, Transcoder: &fakeTranscoder{}})
	c := testChannel()
	c.Items = []Item{
		{Key: first, Title: "Cats in the Museum 2", DurationMS: 12000},
		{Key: second, Title: "Second", DurationMS: 20000},
	}
	saved, err := s.SaveChannel(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Tune(context.Background(), &core.User{ID: 1}, saved.ID, core.ClientCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Programme == nil || info.Programme.Item.Key != second {
		t.Fatalf("tune stayed on the unplayable title: %+v", info.Programme)
	}
	if len(pb.calls) != 2 || pb.calls[0] != first || pb.calls[1] != second {
		t.Fatalf("attempts %v", pb.calls)
	}
}

func TestVirtualTuneUsesFallbackAndBoundsAHungPrepare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Unix(7, 0).UTC()
		first, second, filler := core.MovieKey(1), core.MovieKey(2), core.MovieKey(9)
		prevAttempt, prevBudget := virtualAttemptBudget, virtualTuneBudget
		virtualAttemptBudget = 150 * time.Millisecond
		virtualTuneBudget = time.Second
		t.Cleanup(func() {
			virtualAttemptBudget = prevAttempt
			virtualTuneBudget = prevBudget
		})
		pb := &scriptedPlayback{fakePlayback: &fakePlayback{}, fail: map[core.RatingKey]bool{second: true}, block: first}
		s := testService(t, Options{Now: func() time.Time { return now }, Playback: pb, Transcoder: &fakeTranscoder{}})
		c := testChannel()
		c.Items = []Item{
			{Key: first, Title: "Hung", DurationMS: 12000},
			{Key: second, Title: "Also missing", DurationMS: 20000},
		}
		c.Fallback = []Item{{Key: filler, Title: "Fallback feature", DurationMS: 30 * 60000}}
		saved, err := s.SaveChannel(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		info, err := s.Tune(context.Background(), &core.User{ID: 1}, saved.ID, core.ClientCaps{})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if info.Programme == nil || info.Programme.Item.Title != "Fallback feature" {
			t.Fatalf("fallback was not used: %+v", info.Programme)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("unplayable prepare was not bounded: %s", elapsed)
		}
		if len(pb.calls) > virtualMaxAttempts {
			t.Fatalf("too many attempts: %v", pb.calls)
		}
	})
}

func TestVirtualTuneDoesNotPrepareKnownUnavailableTitles(t *testing.T) {
	now := time.Unix(7, 0).UTC()
	first, second := core.MovieKey(1), core.MovieKey(2)
	pb := &scriptedPlayback{fakePlayback: &fakePlayback{}}
	s := testService(t, Options{
		Now:          func() time.Time { return now },
		Playback:     pb,
		Transcoder:   &fakeTranscoder{},
		Availability: availFunc(func(key core.RatingKey) (bool, bool) { return key == first, key != first }),
	})
	c := testChannel()
	c.Items = []Item{
		{Key: first, Title: "Known missing", DurationMS: 12000},
		{Key: second, Title: "Second", DurationMS: 20000},
	}
	saved, err := s.SaveChannel(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Tune(context.Background(), &core.User{ID: 1}, saved.ID, core.ClientCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Programme == nil || info.Programme.Item.Key != second {
		t.Fatalf("programme %+v", info.Programme)
	}
	for _, key := range pb.calls {
		if key == first {
			t.Fatal("known-unavailable title was prepared")
		}
	}
}
