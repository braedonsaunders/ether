package app

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
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
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

func TestRowViewBatchedTitlesKeepFallbackAndGuideScope(t *testing.T) {
	ctx := context.Background()
	u := &core.User{ID: 7}
	movie := core.Title{Key: core.MovieKey(4), Type: core.TypeMovie, HomeReleaseChecked: true}
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
	one := func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		legacyCalls++
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: season, Type: core.TypeSeason}}, nil
	}
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		admissions++
		copy := movie
		copy.HomeReleaseFallbackAdmitted = true
		return &copy, nil
	}
	vis := experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
	live := func(context.Context, *core.User) ([]frontend.RowEntry, error) {
		guides++
		return []frontend.RowEntry{{Kind: frontend.RowEntryLiveChannel, LiveChannel: &frontend.RowLiveChannel{ID: "channel"}}}, nil
	}
	got, err := rowViewResolveBatchTitles(ctx, u, core.Device{ID: "d"}, "jellyfin", refs, one, batch, get, vis, live)
	if err != nil || len(got) != len(refs) || got[1].Title.Key != season || legacyCalls != 1 || admissions != 1 || guides != 1 {
		t.Fatalf("result len=%d legacy=%d admission=%d guides=%d err=%v", len(got), legacyCalls, admissions, guides, err)
	}
}

func TestRowViewBatchMatchesIndependentLegacy(t *testing.T) {
	// Two independently migrated stores are reset before every case. Each
	// case also constructs a new catalog so no in-process admission cache
	// or title cache can leak across cases.
	legacyDB, candidateDB := store.OpenTest(t), store.OpenTest(t)
	for _, mode := range []string{"admission", "virtual", "missing", "batch_error"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			u := &core.User{ID: 7}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			makeSource := func(db *sql.DB) (*catalog.Catalog, func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error), func(context.Context, *core.User, core.RatingKey) (*core.Title, error)) {
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
				get := func(ctx context.Context, _ *core.User, key core.RatingKey) (*core.Title, error) {
					return c.CachedGet(ctx, key)
				}
				one := func(ctx context.Context, _ *core.User, _ core.Device, _ core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
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
				return c, one, get
			}
			_, legacy, _ := makeSource(legacyDB)
			candidate, one, get := makeSource(candidateDB)
			key := core.MovieKey(10)
			if mode == "virtual" {
				key = core.SeasonKey(99, 1)
			}
			if mode == "missing" {
				key = core.MovieKey(999)
			}
			refs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: key.String()}}
			live := func(context.Context, *core.User) ([]frontend.RowEntry, error) { return nil, core.ErrNotFound }
			want, werr := rowViewResolveBatch(ctx, u, core.Device{ID: "d"}, "jellyfin", refs, legacy, live)
			batch := candidate.CachedGetMany
			if mode == "batch_error" {
				batch = func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
					return nil, errors.New("injected batch SQL failure")
				}
			}
			vis := experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
			got, gerr := rowViewResolveBatchTitles(ctx, u, core.Device{ID: "d"}, "jellyfin", refs, one, batch, get, vis, live)
			if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(got, want) {
				t.Fatalf("legacy=%+v %v batch=%+v %v", want, werr, got, gerr)
			}
		})
	}
}

func TestRowViewBatchAdmissionFailureAndParentReadFallback(t *testing.T) {
	ctx := context.Background()
	u := &core.User{ID: 7}
	movie := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, HomeReleaseChecked: true}
	show := core.Title{Key: core.ShowKey(2), Type: core.TypeShow}
	ep := core.Title{Key: core.EpisodeKey(2, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key}
	vis := experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
	live := func(context.Context, *core.User) ([]frontend.RowEntry, error) { return nil, core.ErrNotFound }
	one := func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, ref core.NeutralEntryRef) (frontend.RowEntry, error) {
		if ref.ID == ep.Key.String() {
			return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &ep}, nil
		}
		return frontend.RowEntry{}, core.ErrNotFound
	}
	get := func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
		return nil, errors.New("admission write/read failure")
	}
	batch := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		out := map[core.RatingKey]core.Title{}
		for _, k := range keys {
			if k == movie.Key {
				out[k] = movie
			}
		}
		return out, nil
	}
	refs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: movie.Key.String()}}
	if got, err := rowViewResolveBatchTitles(ctx, u, core.Device{ID: "d"}, "jellyfin", refs, one, batch, get, vis, live); err == nil || len(got) != 0 {
		t.Fatalf("admission failure leaked %+v %v", got, err)
	}
	calls := 0
	batch = func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("parent batch read failed")
		}
		return map[core.RatingKey]core.Title{ep.Key: ep}, nil
	}
	refs[0].ID = ep.Key.String()
	got, err := rowViewResolveBatchTitles(ctx, u, core.Device{ID: "d"}, "jellyfin", refs, one, batch, get, vis, live)
	if err != nil || len(got) != 1 || got[0].Title.Key != ep.Key || calls != 2 {
		t.Fatalf("legacy parent fallback %+v calls=%d err=%v", got, calls, err)
	}
}

type rowAdmissionDriver struct {
	base    driver.Driver
	fault   *atomic.Bool
	pattern string
}

func (d rowAdmissionDriver) Open(name string) (driver.Conn, error) {
	c, e := d.base.Open(name)
	if e != nil {
		return nil, e
	}
	return &rowAdmissionConn{Conn: c, fault: d.fault, pattern: d.pattern}, nil
}

type rowAdmissionConn struct {
	driver.Conn
	fault   *atomic.Bool
	pattern string
}

func (c *rowAdmissionConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, c.pattern) && c.fault.CompareAndSwap(true, false) {
		return nil, errors.New("injected admission failure")
	}
	if d, ok := c.Conn.(driver.QueryerContext); ok {
		return d.QueryContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}
func (c *rowAdmissionConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(q, c.pattern) && c.fault.CompareAndSwap(true, false) {
		return nil, errors.New("injected admission failure")
	}
	if d, ok := c.Conn.(driver.ExecerContext); ok {
		return d.ExecContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}

func TestRowViewBatchAdmissionDatabaseFaultEquivalence(t *testing.T) {
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
				sql.Register("row_admission_"+strconv.Itoa(index)+label, rowAdmissionDriver{base: drv, fault: fault, pattern: pattern})
				db, e := sql.Open("row_admission_"+strconv.Itoa(index)+label, path)
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
			makeOne := func(c *catalog.Catalog) func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
				return func(ctx context.Context, _ *core.User, _ core.Device, _ core.Protocol, _ core.NeutralEntryRef) (frontend.RowEntry, error) {
					title, e := c.CachedGet(ctx, key)
					if e != nil {
						return frontend.RowEntry{}, e
					}
					return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: title}, nil
				}
			}
			refs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: key.String()}}
			live := func(context.Context, *core.User) ([]frontend.RowEntry, error) { return nil, core.ErrNotFound }
			device := core.Device{ID: "d"}
			want, we := rowViewResolveBatch(ctx, viewer, device, "jellyfin", refs, makeOne(legacy), live)
			get := func(ctx context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
				return candidate.CachedGet(ctx, k)
			}
			got, ge := rowViewResolveBatchTitles(ctx, viewer, device, "jellyfin", refs, makeOne(candidate), candidate.CachedGetMany, get, experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, live)
			if lf.Load() || cf.Load() || !errors.Is(we, core.ErrNotFound) || !errors.Is(ge, core.ErrNotFound) || !reflect.DeepEqual(want, got) {
				t.Fatalf("fault not equivalent legacy=%+v %v candidate=%+v %v fired=%v/%v", want, we, got, ge, !lf.Load(), !cf.Load())
			}
		})
	}
}

func TestRowViewBatchedInheritedMovieUsesAdmission(t *testing.T) {
	ctx := context.Background()
	u := &core.User{ID: 7}
	parent := core.Title{Key: core.MovieKey(10), Type: core.TypeMovie, HomeReleaseChecked: true}
	child := core.Title{Key: core.EpisodeKey(2, 1, 1), Type: core.TypeEpisode, ShowKey: parent.Key}
	batch := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		out := map[core.RatingKey]core.Title{}
		for _, key := range keys {
			if key == parent.Key {
				out[key] = parent
			}
			if key == child.Key {
				out[key] = child
			}
		}
		return out, nil
	}
	attempts := 0
	get := func(_ context.Context, _ *core.User, key core.RatingKey) (*core.Title, error) {
		attempts++
		if key != parent.Key {
			t.Fatal("unexpected admission key")
		}
		return nil, errors.New("admission failed")
	}
	one := func(context.Context, *core.User, core.Device, core.Protocol, core.NeutralEntryRef) (frontend.RowEntry, error) {
		return frontend.RowEntry{}, core.ErrNotFound
	}
	live := func(context.Context, *core.User) ([]frontend.RowEntry, error) { return nil, core.ErrNotFound }
	refs := []core.NeutralEntryRef{{Kind: string(frontend.RowEntryTitle), ID: child.Key.String()}}
	if got, err := rowViewResolveBatchTitles(ctx, u, core.Device{ID: "d"}, "jellyfin", refs, one, batch, get, experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, live); err == nil || len(got) != 0 || attempts != 1 {
		t.Fatalf("admission bypass: %+v attempts=%d err=%v", got, attempts, err)
	}
}

func TestRowViewCallbackAdaptersStayOnTheRequest(t *testing.T) {
	ctx := context.Background()
	ref := core.NeutralEntryRef{Kind: string(frontend.RowEntryTitle), ID: core.MovieKey(3).String()}
	season := core.SeasonKey(9, 1)
	seasonRef := core.NeutralEntryRef{Kind: string(frontend.RowEntryTitle), ID: season.String()}
	var seen []string
	one := func(_ context.Context, u *core.User, d core.Device, p core.Protocol, got core.NeutralEntryRef) (frontend.RowEntry, error) {
		seen = append(seen, strconv.FormatInt(u.ID, 10)+"/"+d.ID+"/"+string(p)+"/"+got.ID)
		key, err := core.ParseRatingKey(got.ID)
		if err != nil {
			return frontend.RowEntry{}, err
		}
		kind := core.TypeMovie
		if key.Kind() == core.KindSeason {
			kind = core.TypeSeason
		}
		return frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &core.Title{Key: key, Type: kind}}, nil
	}
	live := func(context.Context, *core.User) ([]frontend.RowEntry, error) {
		t.Fatal("guide loaded")
		return nil, nil
	}
	first, second := &core.User{ID: 1}, &core.User{ID: 2}
	if _, err := rowViewResolveBatch(ctx, first, core.Device{ID: "tv"}, "jellyfin", []core.NeutralEntryRef{ref}, one, live); err != nil {
		t.Fatal(err)
	}
	if _, err := rowViewResolveBatch(ctx, second, core.Device{ID: "phone"}, "emby", []core.NeutralEntryRef{ref}, one, live); err != nil {
		t.Fatal(err)
	}
	batch := func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return map[core.RatingKey]core.Title{}, nil
	}
	get := func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
		t.Fatal("admission read")
		return nil, nil
	}
	vis := experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}
	if _, err := rowViewResolveBatchTitles(ctx, first, core.Device{ID: "tv"}, "jellyfin", []core.NeutralEntryRef{seasonRef}, one, batch, get, vis, live); err != nil {
		t.Fatal(err)
	}
	if _, err := rowViewResolveBatchTitles(ctx, second, core.Device{ID: "phone"}, "emby", []core.NeutralEntryRef{seasonRef}, one, batch, get, vis, live); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"1/tv/jellyfin/" + ref.ID,
		"2/phone/emby/" + ref.ID,
		"1/tv/jellyfin/" + seasonRef.ID,
		"2/phone/emby/" + seasonRef.ID,
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("adapters reused another request: %v", seen)
	}
	var viewers []int64
	source := &rowViewViewerSource{items: []core.Title{{Key: core.MovieKey(3)}}}
	source.seen = &viewers
	members := []core.NeutralEntryRef{ref}
	if !rowViewCollectionMembers(ctx, source, first, core.CollectionKey(7), members) || !rowViewCollectionMembers(ctx, source, second, core.CollectionKey(8), members) {
		t.Fatal("membership rejected")
	}
	if !reflect.DeepEqual(viewers, []int64{1, 2}) {
		t.Fatalf("collection adapter viewers %v", viewers)
	}
}

type rowViewViewerSource struct {
	items []core.Title
	seen  *[]int64
}

func (s *rowViewViewerSource) Items(_ context.Context, u *core.User, _ core.RatingKey, offset, limit int) (core.Page, error) {
	*s.seen = append(*s.seen, u.ID)
	start := min(offset, len(s.items))
	end := min(start+limit, len(s.items))
	return core.Page{Items: s.items[start:end], Total: len(s.items)}, nil
}
