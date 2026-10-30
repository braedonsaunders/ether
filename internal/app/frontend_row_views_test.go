package app

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type rowViewCollectionsFake struct {
	items []core.Title
	err   error
	calls int
}

func (f *rowViewCollectionsFake) Items(_ context.Context, _ *core.User, _ core.RatingKey, offset, limit int) (core.Page, error) {
	f.calls++
	if f.err != nil {
		return core.Page{}, f.err
	}
	start := min(offset, len(f.items))
	end := min(start+limit, len(f.items))
	return core.Page{Items: f.items[start:end], Total: len(f.items)}, nil
}

type rowViewProductionSource struct{ title core.Title }

func (s rowViewProductionSource) Rows(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
	return []frontend.Row{{ID: "test", Kind: "taste", Purpose: frontend.RowRecommendation, Entries: []frontend.RowEntry{{Kind: frontend.RowEntryTitle, Title: &s.title}}}}, nil
}

func TestProductionRowViewsUsesPolicyAndInvalidatesPreferenceRevision(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 321, Username: "owner"}, "token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(42), Type: core.TypeMovie, IDs: core.IDs{TMDB: 42}, Title: "Visible", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	// Old detail data must still be revalidated from the cache without a
	// network refresh during row-view listing or paging.
	if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, int64(1), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	libraries := []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}}
	visibility := experienceVisibility(a.safety, a.catalog.Get, nil, nil, libraries)
	receipts, err := experience.NewReceipts(experience.ReceiptOptions{DB: a.db, TTL: time.Minute, MaxPerDevice: 100, Active: a.users.ActiveUser, Allowed: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject) bool {
		return true
	}, AllowedAction: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject, frontend.RowInteractionKind) bool {
		return true
	}, Record: func(context.Context, *core.User, core.Device, core.Protocol, experience.ReceiptSubject, frontend.RowInteractionKind) error {
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := experience.NewRowsService(experience.RowServiceDeps{Source: rowViewProductionSource{title}, Visibility: visibility, Receipts: receipts, RankedSource: true, Active: func(context.Context, *core.User) bool { return true }, AllowRow: func(context.Context, *core.User, frontend.Row) bool { return true }, AllowEntry: func(context.Context, *core.User, frontend.RowEntry) bool { return true }, Situation: func(ctx context.Context, _ frontend.RowRequest) (core.HomeSituation, error) {
		return ctx.Value(experienceSituationKey{}).(core.HomeSituation), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := a.newRowViews("jellyfin", rows, receipts, visibility, libraries)
	if err != nil {
		t.Fatal(err)
	}
	otherNode, err := a.newRowViews("jellyfin", rows, receipts, visibility, libraries)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv"}
	views, err := service.ListRowViews(ctx, frontend.RowViewRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Placement: frontend.PlacementHome})
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	var count int
	if err = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM experience_receipts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("listing issued receipts: %d %v", count, err)
	}
	query := frontend.RowViewPageRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", ID: views[0].ID, Limit: 1}
	page, err := service.ResolveRowView(ctx, query)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Title.Key != title.Key || page.ImpressionHandle == "" {
		t.Fatal(page, err)
	}
	if _, err = a.db.ExecContext(ctx, `INSERT INTO notinterested_titles(viewer_id,title_key,added_at,handle) VALUES(?,?,?,?)`, viewer.ID, title.Key.String(), time.Now().UnixNano(), "revision-test"); err != nil {
		t.Fatal(err)
	}
	if _, err = otherNode.ResolveRowView(ctx, query); !errors.Is(err, experience.ErrRowViewStale) {
		t.Fatalf("preference change left snapshot valid: %v", err)
	}
}

func TestRowViewCollectionRevalidatesCurrentMembership(t *testing.T) {
	first, second := core.MovieKey(1), core.MovieKey(2)
	refs := []core.NeutralEntryRef{{Kind: "title", ID: first.String()}, {Kind: "title", ID: second.String()}}
	source := &rowViewCollectionsFake{items: []core.Title{{Key: first}, {Key: second}}}
	viewer := &core.User{ID: 1}
	if !rowViewCollectionMembers(context.Background(), source, viewer, core.CollectionKey(7), refs) {
		t.Fatal("current members rejected")
	}
	for id := int64(3); id <= 150; id++ {
		source.items = append(source.items, core.Title{Key: core.MovieKey(id)})
	}
	if !rowViewCollectionMembers(context.Background(), source, viewer, core.CollectionKey(7), []core.NeutralEntryRef{{Kind: "title", ID: core.MovieKey(150).String()}}) || source.calls < 3 {
		t.Fatal("membership on later page rejected")
	}
	source.items = source.items[:1]
	if rowViewCollectionMembers(context.Background(), source, viewer, core.CollectionKey(7), refs) {
		t.Fatal("removed member accepted")
	}
	source.err = errors.New("offline")
	if rowViewCollectionMembers(context.Background(), source, viewer, core.CollectionKey(7), refs) {
		t.Fatal("lookup failure accepted")
	}
}

func TestRowViewLiveRehydrationLoadsGuideOncePerFreshPass(t *testing.T) {
	var refs []core.NeutralEntryRef
	var channels []frontend.RowEntry
	for i := 0; i < 50; i++ {
		id := strconv.Itoa(i)
		refs = append(refs, core.NeutralEntryRef{Kind: string(frontend.RowEntryLiveChannel), ID: id})
		channels = append(channels, frontend.RowEntry{Kind: frontend.RowEntryLiveChannel, LiveChannel: &frontend.RowLiveChannel{ID: id}})
	}
	calls := 0
	load := func(context.Context, *core.User) ([]frontend.RowEntry, error) { calls++; return channels, nil }
	one := func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
		t.Fatal("live entry performed individual lookup")
		return frontend.RowEntry{}, nil
	}
	u, d := &core.User{ID: 1}, core.Device{ID: "tv"}
	got, err := rowViewResolveBatch(context.Background(), u, d, "jellyfin", refs, one, load)
	if err != nil || len(got) != len(refs) || calls != 1 {
		t.Fatalf("first pass %d entries, %d guide calls: %v", len(got), calls, err)
	}
	channels = channels[:49]
	if _, err = rowViewResolveBatch(context.Background(), u, d, "jellyfin", refs, one, load); err == nil || calls != 2 {
		t.Fatalf("revocation not rechecked: calls=%d err=%v", calls, err)
	}
}

func TestRowViewBatchedTitlePolicyRevalidates(t *testing.T) {
	ctx := context.Background()
	u := &core.User{ID: 7, MaxContentRating: "PG"}
	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow, ContentRating: "TV-G"}
	ep := core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}
	movie := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, ContentRating: "PG"}
	records := map[core.RatingKey]core.Title{show.Key: show, ep.Key: ep, movie.Key: movie}
	batchCalls := 0
	batch := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		batchCalls++
		if len(keys) > 200 {
			t.Fatal("unbounded batch")
		}
		out := map[core.RatingKey]core.Title{}
		for _, key := range keys {
			if x, ok := records[key]; ok {
				out[key] = x
			}
		}
		return out, nil
	}
	one := func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
		return frontend.RowEntry{}, core.ErrNotFound
	}
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		v, ok := records[k]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &v, nil
	}
	vis := experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }}
	refs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: ep.Key.String()}, {Kind: string(frontend.RowEntryTitle), ID: movie.Key.String()}}
	live := func(context.Context, *core.User) ([]frontend.RowEntry, error) {
		t.Fatal("unexpected live lookup")
		return nil, nil
	}
	resolve := func() ([]frontend.RowEntry, error) {
		return rowViewResolveBatchTitles(ctx, u, core.Device{ID: "d"}, "jellyfin", refs, one, batch, get, vis, live)
	}
	got, err := resolve()
	if err != nil || len(got) != 2 || got[0].Title.Key != ep.Key || got[1].Title.Key != movie.Key || batchCalls != 2 {
		t.Fatalf("entries %+v calls=%d err=%v", got, batchCalls, err)
	}
	show.ContentRating = "TV-MA"
	records[show.Key] = show
	if _, err := resolve(); err == nil {
		t.Fatal("changed parent policy was not revalidated")
	}
	delete(records, show.Key)
	if _, err := resolve(); err == nil {
		t.Fatal("missing parent accepted")
	}
}
