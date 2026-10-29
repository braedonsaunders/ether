package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type paritySource struct {
	calls *int
	rows  []frontend.Row
}

func (s paritySource) Rows(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
	*s.calls++
	return s.rows, nil
}
func TestPlexRowsRequiresAuthorization(t *testing.T) {
	ctx := context.Background()
	calls := 0
	if _, err := NewPlexRowsService(PlexRowDeps{}); err == nil {
		t.Fatal("accepted missing authorization")
	}
	title := &core.Title{Key: core.MovieKey(1)}
	d := PlexRowDeps{Source: paritySource{&calls, []frontend.Row{{ID: "a", Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: title}}}, {ID: "a"}, {ID: "empty"}}}, Active: func(_ context.Context, u *core.User) bool { return u.ID == 1 }, AllowRow: func(_ context.Context, _ *core.User, r frontend.Row) bool { return r.ID != "bad" }, AllowEntry: func(_ context.Context, _ *core.User, e frontend.RowEntry) bool { return e.Title == title }, Layout: func(_ context.Context, _ frontend.RowRequest, r []frontend.Row) ([]frontend.Row, error) {
		calls++
		return r, nil
	}}
	svc, err := NewPlexRowsService(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Rows(ctx, frontend.RowRequest{}, "plex"); err == nil {
		t.Fatal("accepted nil viewer")
	}
	if _, err = svc.Rows(ctx, frontend.RowRequest{Viewer: &core.User{ID: 2}}, "plex"); err == nil {
		t.Fatal("accepted inactive viewer")
	}
	if _, err = svc.Rows(ctx, frontend.RowRequest{Viewer: &core.User{ID: 1}}, "jellyfin"); err == nil {
		t.Fatal("accepted other protocol")
	}
	rows, err := svc.Rows(ctx, frontend.RowRequest{Viewer: &core.User{ID: 1}}, "plex")
	if err != nil || calls != 2 || len(rows) != 3 {
		t.Fatalf("rows=%v calls=%d err=%v", rows, calls, err)
	}
	d.Source = paritySource{&calls, []frontend.Row{{ID: "a", Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: core.MovieKey(2)}}}}}}
	svc, _ = NewPlexRowsService(d)
	if _, err = svc.Rows(ctx, frontend.RowRequest{Viewer: &core.User{ID: 1}}, "plex"); err == nil {
		t.Fatal("accepted unauthorized source entry")
	}
	d.Source = paritySource{&calls, []frontend.Row{{ID: "bad"}}}
	svc, _ = NewPlexRowsService(d)
	if _, err = svc.Rows(ctx, frontend.RowRequest{Viewer: &core.User{ID: 1}}, "plex"); err == nil {
		t.Fatal("accepted unauthorized row")
	}
	d.Source = paritySource{&calls, []frontend.Row{{ID: "a"}}}
	d.Layout = func(_ context.Context, _ frontend.RowRequest, _ []frontend.Row) ([]frontend.Row, error) {
		return []frontend.Row{{ID: "bad"}}, nil
	}
	svc, _ = NewPlexRowsService(d)
	if _, err = svc.Rows(ctx, frontend.RowRequest{Viewer: &core.User{ID: 1}}, "plex"); err == nil {
		t.Fatal("accepted unauthorized layout row")
	}

}
