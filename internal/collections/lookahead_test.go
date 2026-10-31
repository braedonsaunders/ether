package collections_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestLookAheadsListEnabledUpcomingCollections(t *testing.T) {
	e := newEnv(t)
	ahead := spec("Look Ahead", "movie", catalogSource())
	ahead.ReleaseMode = "upcoming"
	ahead.LookaheadDays = 30
	saved, err := e.svc.CreateSpec(e.ctx, ahead)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Rebuild(e.ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateSpec(e.ctx, spec("Released only", "movie", catalogSource())); err != nil {
		t.Fatal(err)
	}
	off := spec("Switched off", "movie", catalogSource())
	off.ReleaseMode = "mixed"
	off.LookaheadDays = 30
	off.Enabled = false
	if _, err := e.svc.CreateSpec(e.ctx, off); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.LookAheads(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Look Ahead" {
		t.Fatalf("look aheads: %+v", got)
	}
	page, err := e.svc.Items(e.ctx, nil, core.SmartKey(mustInt(t, saved.ID)), 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Members) == 0 || len(got[0].Members) != page.Total {
		t.Fatalf("members %v, collection lists %d", got[0].Members, page.Total)
	}
}

func TestTitleDetailInspectsUnreleasedTitlesForTheOwner(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	future := core.Title{Key: core.MovieKey(990002), Type: core.TypeMovie, IDs: core.IDs{TMDB: 990002}, Title: "Not Out Yet", Released: time.Now().AddDate(0, 1, 0)}
	adult := core.Title{Key: core.MovieKey(990003), Type: core.TypeMovie, IDs: core.IDs{TMDB: 990003}, Title: "Adult", Released: future.Released, Adult: true}
	for _, x := range []core.Title{future, adult} {
		if err := e.cat.UpsertTitle(ctx, x, false); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.svc.TitleDetail(ctx, future.Key)
	if err != nil || got.Title != "Not Out Yet" {
		t.Fatalf("unreleased detail: %+v %v", got, err)
	}
	if _, err := e.svc.TitleDetail(ctx, adult.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("adult detail: %v", err)
	}
	if _, err := e.svc.TitleDetail(ctx, core.MovieKey(990404)); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing detail: %v", err)
	}
}
