package favorites

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSetListAuthorizationAndEvents(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events := 0
	db := store.OpenTest(t)
	s, err := New(Options{DB: db, Marked: state.New(db, state.Options{}).Playlists().(core.TransactionalMarkedPlaylists), Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) {
		if id == 9 {
			return nil, errors.New("revoked")
		}
		return &core.User{ID: id}, nil
	}, Allowed: func(_ context.Context, _ *core.User, sub Subject) bool { return sub.ID != "hidden" }, Changed: func(context.Context, int64, Subject) { events++ }})
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	a := Subject{Kind: KindTitle, ID: core.MovieKey(1).String()}
	b := Subject{Kind: KindPerson, ID: "tmdb:42"}
	if changed, err := s.Set(ctx, u, a, true); err != nil || !changed {
		t.Fatalf("set: %v %v", changed, err)
	}
	if changed, err := s.Set(ctx, u, a, true); err != nil || changed || events != 1 {
		t.Fatal("duplicate write emitted event")
	}
	titleEntries, listErr := s.List(ctx, u, KindTitle, 1)
	if listErr != nil || len(titleEntries) != 1 || titleEntries[0].Subject != a {
		t.Fatalf("title list: %+v %v", titleEntries, listErr)
	}
	if _, err := s.Set(ctx, u, Subject{Kind: KindChannel, ID: "https://example.invalid/secret"}, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("source URL accepted as identity")
	}
	now = now.Add(time.Second)
	if changed, err := s.Set(ctx, u, b, true); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if yes, err := s.Has(ctx, &core.User{ID: 2}, a); err != nil || yes {
		t.Fatal("favorite crossed viewers")
	}
	if _, err := s.Set(ctx, u, Subject{Kind: KindTitle, ID: "hidden"}, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("denied subject accepted")
	}
	if _, err := s.Set(ctx, &core.User{ID: 9}, a, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("revoked viewer accepted")
	}
	entries, err := s.List(ctx, u, KindPerson, 1)
	if err != nil || len(entries) != 1 || entries[0].Subject != b || !entries[0].AddedAt.Equal(now) {
		t.Fatalf("list: %+v %v", entries, err)
	}
	if changed, err := s.Set(ctx, u, a, false); err != nil || !changed || events != 3 {
		t.Fatal(changed, err, events)
	}
	if changed, err := s.Set(ctx, u, a, false); err != nil || changed || events != 3 {
		t.Fatal("duplicate removal emitted event")
	}
}

func TestMirrorEnabledRequiresActiveViewerAndDefaultsOn(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s, err := New(Options{DB: db, Marked: state.New(db, state.Options{}).Playlists().(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) {
		if id == 9 {
			return nil, errors.New("revoked")
		}
		return &core.User{ID: id}, nil
	}, Allowed: func(context.Context, *core.User, Subject) bool { return true }, Changed: func(context.Context, int64, Subject) {}})
	if err != nil {
		t.Fatal(err)
	}
	v := &core.User{ID: 1}
	if got, err := s.MirrorEnabled(ctx, v); err != nil || !got {
		t.Fatalf("default %v %v", got, err)
	}
	if err = s.SetMirror(ctx, v, false); err != nil {
		t.Fatal(err)
	}
	if got, err := s.MirrorEnabled(ctx, v); err != nil || got {
		t.Fatalf("disabled %v %v", got, err)
	}
	if err = s.SetMirror(ctx, v, true); err != nil {
		t.Fatal(err)
	}
	if got, err := s.MirrorEnabled(ctx, v); err != nil || !got {
		t.Fatalf("enabled %v %v", got, err)
	}
	for _, v := range []*core.User{nil, {ID: 0}, {ID: 9}} {
		if _, err := s.MirrorEnabled(ctx, v); !errors.Is(err, ErrForbidden) {
			t.Fatalf("unauthorized: %v", err)
		}
	}
}
