package experience

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRowViewMembersNilSourceErrorAndBound(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 4}
	collection := core.CollectionKey(7)
	first := core.MovieKey(1)
	refs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: first.String()}}
	called := false
	items := func(context.Context, *core.User, core.RatingKey, int, int) (RowViewCollectionPage, error) {
		called = true
		return RowViewCollectionPage{}, nil
	}
	if RowViewMembers(ctx, nil, viewer, collection, refs) || RowViewMembers(ctx, items, nil, collection, refs) || called {
		t.Fatal("nil source or viewer established membership")
	}
	if RowViewMembers(ctx, items, viewer, collection, nil) {
		t.Fatal("empty refs accepted")
	}
	if RowViewMembers(ctx, items, viewer, collection, []core.NeutralEntryRef{{Kind: string(frontend.RowEntryLiveChannel), ID: "x"}}) {
		t.Fatal("non-title ref accepted")
	}
	if RowViewMembers(ctx, items, viewer, collection, []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: "not-a-key"}}) {
		t.Fatal("unparseable ref accepted")
	}
	sourceErr := errors.New("offline")
	sawViewer := int64(0)
	failing := func(ctx context.Context, u *core.User, _ core.RatingKey, _, _ int) (RowViewCollectionPage, error) {
		sawViewer = u.ID
		return RowViewCollectionPage{}, sourceErr
	}
	if RowViewMembers(ctx, failing, viewer, collection, refs) || sawViewer != viewer.ID {
		t.Fatal("source error accepted")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	cancelled := func(ctx context.Context, _ *core.User, _ core.RatingKey, _, _ int) (RowViewCollectionPage, error) {
		return RowViewCollectionPage{}, ctx.Err()
	}
	if RowViewMembers(cancelCtx, cancelled, viewer, collection, refs) {
		t.Fatal("cancelled source accepted")
	}
	pages := func(_ context.Context, _ *core.User, _ core.RatingKey, offset, limit int) (RowViewCollectionPage, error) {
		if offset == 0 {
			return RowViewCollectionPage{Keys: []core.RatingKey{first, core.MovieKey(2)}, Total: 3}, nil
		}
		return RowViewCollectionPage{Keys: []core.RatingKey{core.MovieKey(150)}, Total: 3}, nil
	}
	if !RowViewMembers(ctx, pages, viewer, collection, []core.NeutralEntryRef{{Kind: "title", ID: first.String()}, {Kind: "title", ID: first.String()}}) {
		t.Fatal("repeated member rejected")
	}
	if !RowViewMembers(ctx, pages, viewer, collection, []core.NeutralEntryRef{{Kind: "title", ID: core.MovieKey(150).String()}}) {
		t.Fatal("later page rejected")
	}
	short := func(context.Context, *core.User, core.RatingKey, int, int) (RowViewCollectionPage, error) {
		return RowViewCollectionPage{Keys: []core.RatingKey{first}, Total: 1}, nil
	}
	if RowViewMembers(ctx, short, viewer, collection, []core.NeutralEntryRef{{Kind: "title", ID: first.String()}, {Kind: "title", ID: core.MovieKey(2).String()}}) {
		t.Fatal("removed member accepted")
	}
	var offsets []int
	bounded := func(_ context.Context, _ *core.User, _ core.RatingKey, offset, limit int) (RowViewCollectionPage, error) {
		offsets = append(offsets, offset)
		if offset >= rowViewMemberScanLimit {
			t.Fatal("scan continued past the bound")
		}
		keys := make([]core.RatingKey, limit)
		for i := range keys {
			keys[i] = core.MovieKey(int64(offset + i + 1))
		}
		return RowViewCollectionPage{Keys: keys, Total: rowViewMemberScanLimit + limit}, nil
	}
	missing := core.NeutralEntryRef{Kind: "title", ID: core.MovieKey(1_000_000).String()}
	if RowViewMembers(ctx, bounded, viewer, collection, []core.NeutralEntryRef{missing}) || len(offsets) != rowViewMemberScanLimit/rowViewMemberPage || offsets[len(offsets)-1] != rowViewMemberScanLimit-rowViewMemberPage {
		t.Fatalf("bound not closed: %v", offsets)
	}
}

func TestRowViewHydrateLegacyNilEmptyOrderAndSourceError(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	got, err := RowViewHydrateLegacy(ctx, viewer, nil, nil, nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil refs produced %#v %v", got, err)
	}
	sentinel := errors.New("source down")
	one := func(context.Context, core.NeutralEntryRef) (frontend.RowEntry, error) {
		return frontend.RowEntry{Handle: "minted"}, sentinel
	}
	ref := core.NeutralEntryRef{Kind: string(frontend.RowEntryTitle), ID: core.MovieKey(1).String()}
	got, err = RowViewHydrateLegacy(ctx, viewer, []core.NeutralEntryRef{ref}, one, nil)
	if got != nil || !errors.Is(err, sentinel) {
		t.Fatalf("source error became %#v %v", got, err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	one = func(ctx context.Context, _ core.NeutralEntryRef) (frontend.RowEntry, error) {
		return frontend.RowEntry{}, ctx.Err()
	}
	got, err = RowViewHydrateLegacy(cancelCtx, viewer, []core.NeutralEntryRef{ref}, one, nil)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation became %#v %v", got, err)
	}
	guides := 0
	guide := func(context.Context) ([]frontend.RowEntry, error) {
		guides++
		return []frontend.RowEntry{{Kind: frontend.RowEntryLiveChannel, LiveChannel: &frontend.RowLiveChannel{ID: "a"}, Handle: "guide"}}, nil
	}
	refs := []core.NeutralEntryRef{
		{Kind: string(frontend.RowEntryLiveChannel), ID: "a"},
		{Kind: string(frontend.RowEntryLiveChannel), ID: "a"},
		{Kind: string(frontend.RowEntryTitle), ID: core.MovieKey(8).String()},
	}
	one = func(_ context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		key, _ := core.ParseRatingKey(ref.ID)
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Handle: "legacy", Title: &core.Title{Key: key}}, nil
	}
	got, err = RowViewHydrateLegacy(ctx, viewer, refs, one, guide)
	if err != nil || guides != 1 || len(got) != 3 || got[0].LiveChannel.ID != "a" || got[1].LiveChannel.ID != "a" || got[2].Title.Key != core.MovieKey(8) || got[2].Handle != "legacy" || got[0].Handle != "guide" {
		t.Fatalf("order/guide %#v guides=%d err=%v", got, guides, err)
	}
	got, err = RowViewHydrateLegacy(ctx, viewer, refs[:1], one, guide)
	if err != nil || guides != 2 {
		t.Fatalf("guide shared across passes: %d %v", guides, err)
	}
	guide = func(context.Context) ([]frontend.RowEntry, error) { return nil, sentinel }
	got, err = RowViewHydrateLegacy(ctx, viewer, refs[:1], one, guide)
	if got != nil || !errors.Is(err, sentinel) {
		t.Fatalf("guide error became %#v %v", got, err)
	}
}

func TestRowViewHydrateEntriesNilEmptyOrderPolicyAndFallback(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7, MaxContentRating: "PG"}
	got, err := RowViewHydrateEntries(ctx, viewer, nil, nil, nil, nil, Visibility{}, nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty success %#v %v", got, err)
	}
	movie := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Batch", ContentRating: "PG"}
	records := map[core.RatingKey]core.Title{movie.Key: movie}
	var batches [][]core.RatingKey
	batch := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		batches = append(batches, append([]core.RatingKey(nil), keys...))
		if len(keys) == 1 && keys[0] == core.MovieKey(404) {
			return nil, nil
		}
		out := map[core.RatingKey]core.Title{}
		for _, key := range keys {
			if title, ok := records[key]; ok {
				out[key] = title
			}
		}
		return out, nil
	}
	one := func(context.Context, core.NeutralEntryRef) (frontend.RowEntry, error) {
		t.Fatal("title batch used legacy")
		return frontend.RowEntry{}, nil
	}
	read := func(context.Context, core.RatingKey) (*core.Title, error) {
		t.Fatal("unexpected read")
		return nil, nil
	}
	vis := Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
	refs := []core.NeutralEntryRef{
		{Kind: string(frontend.RowEntryTitle), ID: movie.Key.String()},
		{Kind: string(frontend.RowEntryTitle), ID: movie.Key.String()},
	}
	got, err = RowViewHydrateEntries(ctx, viewer, refs, one, batch, read, vis, nil)
	if err != nil || vis.GetTitle != nil || len(got) != 2 || got[0].Title.Key != movie.Key || got[1].Title.Key != movie.Key || got[0].Handle != "" || got[0].Title == got[1].Title {
		t.Fatalf("repeated refs %#v err=%v", got, err)
	}
	got[0].Title.Title = "Changed"
	if got[1].Title.Title != "Batch" {
		t.Fatal("title copies alias the batch record")
	}
	missing := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: core.MovieKey(404).String()}}
	got, err = RowViewHydrateEntries(ctx, viewer, missing, one, batch, read, vis, nil)
	if got != nil || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("nil batch map became %#v %v", got, err)
	}
	bad := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: "bad"}}
	got, err = RowViewHydrateEntries(ctx, viewer, bad, one, func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		t.Fatal("invalid ref reached the batch")
		return nil, nil
	}, read, vis, nil)
	if got != nil || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("invalid ref %#v %v", got, err)
	}

	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow, ContentRating: "TV-G"}
	ep1 := core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}
	ep2 := core.Title{Key: core.EpisodeKey(1, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}
	other := core.Title{Key: core.ShowKey(2), Type: core.TypeShow, ContentRating: "TV-G"}
	ep3 := core.Title{Key: core.EpisodeKey(2, 1, 1), Type: core.TypeEpisode, ShowKey: other.Key, ContentRating: "TV-G"}
	records = map[core.RatingKey]core.Title{show.Key: show, other.Key: other, ep1.Key: ep1, ep2.Key: ep2, ep3.Key: ep3}
	batches = nil
	policyRefs := []core.NeutralEntryRef{
		{Kind: string(frontend.RowEntryTitle), ID: ep1.Key.String()},
		{Kind: string(frontend.RowEntryTitle), ID: ep2.Key.String()},
		{Kind: string(frontend.RowEntryTitle), ID: ep3.Key.String()},
	}
	got, err = RowViewHydrateEntries(ctx, viewer, policyRefs, one, batch, read, vis, nil)
	if err != nil || len(got) != 3 || len(batches) != 2 || !reflect.DeepEqual(batches[0], []core.RatingKey{ep1.Key, ep2.Key, ep3.Key}) || !reflect.DeepEqual(batches[1], []core.RatingKey{show.Key, other.Key}) {
		t.Fatalf("parent order batches=%v got=%v err=%v", batches, got, err)
	}
	show.ContentRating = "TV-MA"
	records[show.Key] = show
	got, err = RowViewHydrateEntries(ctx, viewer, policyRefs[:1], one, batch, read, vis, nil)
	if got != nil || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden parent %#v %v", got, err)
	}
	delete(records, show.Key)
	got, err = RowViewHydrateEntries(ctx, viewer, policyRefs[:1], one, batch, read, vis, nil)
	if got != nil || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing parent %#v %v", got, err)
	}

	batchErr := errors.New("batch sql")
	legacyErr := errors.New("legacy sql")
	fallbackRefs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: movie.Key.String()}}
	got, err = RowViewHydrateEntries(ctx, viewer, fallbackRefs, func(context.Context, core.NeutralEntryRef) (frontend.RowEntry, error) {
		return frontend.RowEntry{}, legacyErr
	}, func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return nil, batchErr
	}, read, vis, func(context.Context) ([]frontend.RowEntry, error) {
		t.Fatal("guide loaded")
		return nil, nil
	})
	if got != nil || !errors.Is(err, legacyErr) || errors.Is(err, batchErr) {
		t.Fatalf("fallback error %#v %v", got, err)
	}
}

func TestRowViewHydrateEntriesFallbackMemoVirtualAndAdmission(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7}
	movie := core.Title{Key: core.MovieKey(4), Type: core.TypeMovie, Title: "Batch", HomeReleaseChecked: true}
	season := core.SeasonKey(5, 1)
	refs := make([]core.NeutralEntryRef, 0, 202)
	refs = append(refs, core.NeutralEntryRef{Kind: string(frontend.RowEntryTitle), ID: movie.Key.String()}, core.NeutralEntryRef{Kind: string(frontend.RowEntryTitle), ID: season.String()})
	for i := 0; i < 200; i++ {
		refs = append(refs, core.NeutralEntryRef{Kind: string(frontend.RowEntryLiveChannel), ID: "channel"})
	}
	batch := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		out := map[core.RatingKey]core.Title{}
		for _, key := range keys {
			if key == movie.Key {
				out[key] = movie
			}
		}
		return out, nil
	}
	legacyCalls, admissions, guides := 0, 0, 0
	one := func(_ context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		legacyCalls++
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Handle: "pass", Title: &core.Title{Key: season, Type: core.TypeSeason}}, nil
	}
	read := func(context.Context, core.RatingKey) (*core.Title, error) {
		admissions++
		copy := movie
		copy.HomeReleaseFallbackAdmitted = true
		copy.Title = "Admitted"
		return &copy, nil
	}
	vis := Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
	guide := func(context.Context) ([]frontend.RowEntry, error) {
		guides++
		return []frontend.RowEntry{{Kind: frontend.RowEntryLiveChannel, LiveChannel: &frontend.RowLiveChannel{ID: "channel"}}}, nil
	}
	got, err := RowViewHydrateEntries(ctx, viewer, refs, one, batch, read, vis, guide)
	if err != nil || len(got) != len(refs) || got[0].Title.Title != "Admitted" || got[0].Handle != "" || got[1].Title.Key != season || got[1].Handle != "pass" || legacyCalls != 1 || admissions != 1 || guides != 1 || vis.GetTitle != nil {
		t.Fatalf("len=%d legacy=%d admission=%d guides=%d err=%v", len(got), legacyCalls, admissions, guides, err)
	}
	guides = 0
	if _, err = RowViewHydrateEntries(ctx, &core.User{ID: 8}, refs[2:3], one, batch, read, vis, guide); err != nil || guides != 1 {
		t.Fatalf("guide memo escaped the pass: guides=%d err=%v", guides, err)
	}

	read = func(context.Context, core.RatingKey) (*core.Title, error) {
		return nil, errors.New("admission write/read failure")
	}
	one = func(context.Context, core.NeutralEntryRef) (frontend.RowEntry, error) {
		t.Fatal("admission failure fell back to legacy")
		return frontend.RowEntry{}, nil
	}
	got, err = RowViewHydrateEntries(ctx, viewer, refs[:1], one, batch, read, vis, guide)
	if got != nil || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("admission failure %#v %v", got, err)
	}

	calls := 0
	ep := core.Title{Key: core.EpisodeKey(2, 1, 1), Type: core.TypeEpisode, ShowKey: core.ShowKey(2)}
	batch = func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("parent batch read failed")
		}
		return map[core.RatingKey]core.Title{ep.Key: ep}, nil
	}
	one = func(_ context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &ep}, nil
	}
	parentRefs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: ep.Key.String()}}
	got, err = RowViewHydrateEntries(ctx, viewer, parentRefs, one, batch, read, vis, func(context.Context) ([]frontend.RowEntry, error) {
		return nil, core.ErrNotFound
	})
	if err != nil || len(got) != 1 || got[0].Title.Key != ep.Key || calls != 2 {
		t.Fatalf("parent fallback %+v calls=%d err=%v", got, calls, err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	legacyCalls = 0
	batch = func(ctx context.Context, _ []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return nil, ctx.Err()
	}
	one = func(context.Context, core.NeutralEntryRef) (frontend.RowEntry, error) {
		legacyCalls++
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &movie}, nil
	}
	got, err = RowViewHydrateEntries(cancelCtx, viewer, refs[:1], one, batch, read, vis, func(context.Context) ([]frontend.RowEntry, error) {
		t.Fatal("cancelled batch loaded the guide")
		return nil, nil
	})
	if err != nil || got == nil || len(got) != 1 || got[0].Title.Key != movie.Key || legacyCalls != 1 {
		t.Fatalf("cancelled batch did not fall back: %#v calls=%d err=%v", got, legacyCalls, err)
	}
}

func TestRowViewHydrateEntriesSecondChunkUsesFullLegacy(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7}
	refs := make([]core.NeutralEntryRef, 201)
	for i := 0; i < 199; i++ {
		refs[i] = core.NeutralEntryRef{Kind: string(frontend.RowEntryTitle), ID: core.MovieKey(int64(i + 1)).String()}
	}
	refs[199] = core.NeutralEntryRef{Kind: string(frontend.RowEntryLiveChannel), ID: "live"}
	refs[200] = core.NeutralEntryRef{Kind: string(frontend.RowEntryTitle), ID: core.MovieKey(900).String()}
	guides, legacy := 0, 0
	batch := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		for _, key := range keys {
			if key == core.MovieKey(900) {
				return nil, errors.New("second chunk failed")
			}
		}
		out := make(map[core.RatingKey]core.Title, len(keys))
		for _, key := range keys {
			out[key] = core.Title{Key: key, Type: core.TypeMovie, Title: "batch"}
		}
		return out, nil
	}
	one := func(_ context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		legacy++
		key, err := core.ParseRatingKey(ref.ID)
		if err != nil {
			return frontend.RowEntry{}, err
		}
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key, Type: core.TypeMovie, Title: "legacy"}}, nil
	}
	guide := func(context.Context) ([]frontend.RowEntry, error) {
		guides++
		return []frontend.RowEntry{{Kind: frontend.RowEntryLiveChannel, LiveChannel: &frontend.RowLiveChannel{ID: "live"}}}, nil
	}
	vis := Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
	got, err := RowViewHydrateEntries(ctx, viewer, refs, one, batch, func(context.Context, core.RatingKey) (*core.Title, error) {
		t.Fatal("read")
		return nil, nil
	}, vis, guide)
	if err != nil || len(got) != len(refs) || guides != 1 || legacy != 200 || got[0].Title.Title != "legacy" || got[200].Title.Title != "legacy" || got[199].LiveChannel.ID != "live" {
		t.Fatalf("len=%d legacy=%d guides=%d err=%v", len(got), legacy, guides, err)
	}
}

func TestRowViewHydrateEntriesMatchesIndependentLegacy(t *testing.T) {
	legacyDB, candidateDB := store.OpenTest(t), store.OpenTest(t)
	for _, mode := range []string{"admission", "virtual", "missing", "batch_error"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			viewer := &core.User{ID: 7}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			makeSource := func(db *sql.DB) (*catalog.Catalog, RowViewEntry, RowViewTitleRead) {
				for _, table := range []string{"catalog_home_release", "catalog_titles"} {
					if _, e := db.ExecContext(ctx, "DELETE FROM "+table); e != nil {
						t.Fatal(e)
					}
				}
				c, e := catalog.New(db, nil, config.Catalog{GateMoviesByHomeRelease: true}, nil, catalog.WithNow(func() time.Time { return now }))
				if e != nil {
					t.Fatal(e)
				}
				key := core.MovieKey(10)
				title := core.Title{Key: key, Type: core.TypeMovie, Title: "Eligible", Released: now.AddDate(-1, 0, 0)}
				if e = c.UpsertTitle(ctx, title, true); e != nil {
					t.Fatal(e)
				}
				if _, e = db.ExecContext(ctx, `INSERT INTO catalog_home_release(key,checked,earliest_ts,fallback_admitted,attempted_at) VALUES(?,1,0,0,0)`, int64(key)); e != nil {
					t.Fatal(e)
				}
				read := func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
					return c.CachedGet(ctx, key)
				}
				one := func(ctx context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
					key, e := core.ParseRatingKey(ref.ID)
					if e != nil {
						return frontend.RowEntry{}, e
					}
					if mode == "virtual" {
						return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key, Type: core.TypeSeason}}, nil
					}
					title, e := c.CachedGet(ctx, key)
					if e != nil {
						return frontend.RowEntry{}, e
					}
					return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: title}, nil
				}
				return c, one, read
			}
			_, legacy, _ := makeSource(legacyDB)
			candidate, one, read := makeSource(candidateDB)
			key := core.MovieKey(10)
			if mode == "virtual" {
				key = core.SeasonKey(99, 1)
			}
			if mode == "missing" {
				key = core.MovieKey(999)
			}
			refs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: key.String()}}
			guide := func(context.Context) ([]frontend.RowEntry, error) { return nil, core.ErrNotFound }
			want, werr := RowViewHydrateLegacy(ctx, viewer, refs, legacy, guide)
			batch := candidate.CachedGetMany
			if mode == "batch_error" {
				batch = func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
					return nil, errors.New("injected batch SQL failure")
				}
			}
			vis := Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
			got, gerr := RowViewHydrateEntries(ctx, viewer, refs, one, batch, read, vis, guide)
			if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(got, want) || (gerr != nil && got != nil) || (werr != nil && want != nil) {
				t.Fatalf("legacy=%+v %v batch=%+v %v", want, werr, got, gerr)
			}
		})
	}
}

func TestRowViewHydrateEntriesAdmissionDatabaseFaultEquivalence(t *testing.T) {
	for index, pattern := range []string{"UPDATE catalog_home_release SET fallback_admitted", "SELECT fallback_admitted FROM catalog_home_release"} {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			viewer := &core.User{ID: 7}
			key := core.MovieKey(71)
			makeSource := func(label string) (*catalog.Catalog, *atomic.Bool) {
				path := filepath.Join(t.TempDir(), "fixture.db")
				base, e := store.OpenPath(ctx, path)
				if e != nil {
					t.Fatal(e)
				}
				drv := base.Driver()
				base.Close()
				fault := &atomic.Bool{}
				sql.Register("exp_row_hydration_"+strconv.Itoa(index)+label, hydrationAdmissionDriver{base: drv, fault: fault, pattern: pattern})
				db, e := sql.Open("exp_row_hydration_"+strconv.Itoa(index)+label, path)
				if e != nil {
					t.Fatal(e)
				}
				db.SetMaxOpenConns(1)
				t.Cleanup(func() { db.Close() })
				c, e := catalog.New(db, nil, config.Catalog{GateMoviesByHomeRelease: true}, nil, catalog.WithNow(func() time.Time { return now }))
				if e != nil {
					t.Fatal(e)
				}
				if e = c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, Title: "Eligible", Released: now.AddDate(-1, 0, 0)}, true); e != nil {
					t.Fatal(e)
				}
				if _, e = db.ExecContext(ctx, `INSERT INTO catalog_home_release(key,checked,earliest_ts,fallback_admitted,attempted_at) VALUES(?,1,0,0,0)`, int64(key)); e != nil {
					t.Fatal(e)
				}
				return c, fault
			}
			legacy, lf := makeSource("legacy")
			candidate, cf := makeSource("candidate")
			lf.Store(true)
			cf.Store(true)
			makeOne := func(c *catalog.Catalog) RowViewEntry {
				return func(ctx context.Context, _ core.NeutralEntryRef) (frontend.RowEntry, error) {
					title, e := c.CachedGet(ctx, key)
					if e != nil {
						return frontend.RowEntry{}, e
					}
					return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: title}, nil
				}
			}
			refs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: key.String()}}
			guide := func(context.Context) ([]frontend.RowEntry, error) { return nil, core.ErrNotFound }
			want, we := RowViewHydrateLegacy(ctx, viewer, refs, makeOne(legacy), guide)
			read := func(ctx context.Context, k core.RatingKey) (*core.Title, error) {
				return candidate.CachedGet(ctx, k)
			}
			got, ge := RowViewHydrateEntries(ctx, viewer, refs, makeOne(candidate), candidate.CachedGetMany, read, Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, guide)
			if lf.Load() || cf.Load() || got != nil || want != nil || !errors.Is(we, core.ErrNotFound) || !errors.Is(ge, core.ErrNotFound) || !reflect.DeepEqual(want, got) {
				t.Fatalf("fault not equivalent legacy=%+v %v candidate=%+v %v fired=%v/%v", want, we, got, ge, !lf.Load(), !cf.Load())
			}
		})
	}
}

type hydrationAdmissionDriver struct {
	base    driver.Driver
	fault   *atomic.Bool
	pattern string
}

func (d hydrationAdmissionDriver) Open(name string) (driver.Conn, error) {
	c, e := d.base.Open(name)
	if e != nil {
		return nil, e
	}
	return &hydrationAdmissionConn{Conn: c, fault: d.fault, pattern: d.pattern}, nil
}

type hydrationAdmissionConn struct {
	driver.Conn
	fault   *atomic.Bool
	pattern string
}

func (c *hydrationAdmissionConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, c.pattern) && c.fault.CompareAndSwap(true, false) {
		return nil, errors.New("injected admission failure")
	}
	if d, ok := c.Conn.(driver.QueryerContext); ok {
		return d.QueryContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}

func (c *hydrationAdmissionConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(q, c.pattern) && c.fault.CompareAndSwap(true, false) {
		return nil, errors.New("injected admission failure")
	}
	if d, ok := c.Conn.(driver.ExecerContext); ok {
		return d.ExecContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}
