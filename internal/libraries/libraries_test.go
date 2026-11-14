package libraries

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func openService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db, err := store.OpenPath(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := New(context.Background(), db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestSeededBuiltins(t *testing.T) {
	s, _ := openService(t)
	ctx := context.Background()
	libs, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 2 {
		t.Fatalf("List = %+v, want the two builtins", libs)
	}
	movies, shows := libs[0], libs[1]
	if movies.ID != 1 || movies.Key != "movies" || movies.Name != "Movies" || movies.Source != core.LibraryBuiltin || movies.MediaType != core.TypeMovie {
		t.Fatalf("movies = %+v", movies)
	}
	if shows.ID != 2 || shows.Key != "shows" || shows.Name != "TV Shows" || shows.Source != core.LibraryBuiltin || shows.MediaType != core.TypeShow {
		t.Fatalf("shows = %+v", shows)
	}
	if got, err := s.ByKey(ctx, "movies"); err != nil || got.ID != 1 {
		t.Fatalf("ByKey(movies) = %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, 99); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get(99) = %v, want ErrNotFound", err)
	}
	if core.CanonicalLibraryForTitle(core.TypeMovie) != 1 || core.CanonicalLibraryForTitle(core.TypeSeason) != 2 || core.CanonicalLibraryForTitle(core.TypeEpisode) != 2 {
		t.Fatal("canonical libraries wrong")
	}
}

func TestFilterAndPluginLibraries(t *testing.T) {
	s, _ := openService(t)
	ctx := context.Background()
	gen := s.Generation()

	f := core.Filter{Type: core.TypeMovie, Genres: []string{"Comedy"}}
	filter, err := s.Create(ctx, "Funny Movies!", f.Type, libraryRules(f), "hide", core.Presentations{})
	if err != nil {
		t.Fatal(err)
	}
	if filter.ID != 3 || filter.Key != "funny-movies" || filter.Source != core.LibraryCatalog || filter.MediaType != core.TypeMovie {
		t.Fatalf("filter = %+v", filter)
	}
	got, err := s.Get(ctx, filter.ID)
	if err != nil || got.MediaType != core.TypeMovie || len(got.Rules) != 1 || got.Rules[0].Action != "show" {
		t.Fatalf("round-trip filter = %+v, %v", got, err)
	}
	dup, err := s.Create(ctx, "Funny Movies", f.Type, libraryRules(f), "hide", core.Presentations{})
	if err != nil || dup.Key != "funny-movies-2" {
		t.Fatalf("dup key = %+v, %v", dup, err)
	}
	if _, err := s.Create(ctx, "!!!", f.Type, libraryRules(f), "hide", core.Presentations{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unslugifiable name = %v, want ErrInvalid", err)
	}
	if _, err := s.Create(ctx, "Bad", core.TypeEpisode, libraryRules(core.Filter{Type: core.TypeEpisode}), "hide", core.Presentations{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("episode filter = %v, want ErrInvalid", err)
	}

	plugin, err := s.UpsertPluginLibrary(ctx, "fieldrec", "album", PluginLibraryDecl{Name: "Field Recordings"})
	if err != nil {
		t.Fatal(err)
	}
	if plugin.Key != "pk-fieldrec-album" || plugin.Source != core.LibraryPlugin || plugin.PluginID != "fieldrec" || plugin.KindKey != "album" {
		t.Fatalf("plugin = %+v", plugin)
	}
	// An owner rename wins over a later manifest (§2.5).
	if err := s.Rename(ctx, plugin.ID, "My Recordings"); err != nil {
		t.Fatal(err)
	}
	again, err := s.UpsertPluginLibrary(ctx, "fieldrec", "album", PluginLibraryDecl{Name: "Renamed By Plugin"})
	if err != nil || again.ID != plugin.ID || again.Name != "My Recordings" {
		t.Fatalf("upsert after rename = %+v, %v", again, err)
	}
	if _, err := s.UpsertPluginLibrary(ctx, "", "album", PluginLibraryDecl{Name: "X"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty plugin = %v, want ErrInvalid", err)
	}
	if s.Generation() == gen {
		t.Fatal("Generation did not change on mutation")
	}
}

func TestCreateCollectionLibrary(t *testing.T) {
	s, db := openService(t)
	ctx := context.Background()

	lib, err := s.CreateCollection(ctx, "MDBList picks", 42, core.TypeMovie, core.Presentations{})
	if err != nil {
		t.Fatal(err)
	}
	if lib.Source != core.LibraryCollection || lib.CollectionID != 42 || lib.MediaType != core.TypeMovie || lib.Key != "mdblist-picks" {
		t.Fatalf("collection library = %+v", lib)
	}
	// The collection id round-trips through the store.
	var stored int64
	if err := db.QueryRowContext(ctx, `SELECT collection_id FROM libraries WHERE id=?`, int64(lib.ID)).Scan(&stored); err != nil || stored != 42 {
		t.Fatalf("stored collection_id = %d, %v", stored, err)
	}
	got, err := s.Get(ctx, lib.ID)
	if err != nil || got.CollectionID != 42 || got.Source != core.LibraryCollection {
		t.Fatalf("round-trip = %+v, %v", got, err)
	}

	if _, err := s.CreateCollection(ctx, "No source", 0, core.TypeMovie, core.Presentations{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing collection = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateCollection(ctx, "Bad type", 42, core.TypeEpisode, core.Presentations{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("episode type = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateCollection(ctx, "!!!", 42, core.TypeMovie, core.Presentations{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unslugifiable name = %v, want ErrInvalid", err)
	}
	// Collection libraries are console-only like filter ones and may be deleted.
	if lib.Present.Plex != nil || lib.Present.MediaBrowser != nil || lib.Present.Stremio != nil || lib.Present.AdapterOnly {
		t.Fatalf("new library must be console-only: %+v", lib.Present)
	}
	if err := s.Delete(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerEditsAndDelete(t *testing.T) {
	s, _ := openService(t)
	ctx := context.Background()
	if err := s.Rename(ctx, 1, "Films"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSort(ctx, 1, 9); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHidden(ctx, 2, true); err != nil {
		t.Fatal(err)
	}
	present := core.Presentations{AdapterOnly: true}
	if err := s.SetPresent(ctx, 1, present); err != nil {
		t.Fatal(err)
	}
	libs, err := s.List(ctx)
	if err != nil || libs[0].ID != 2 || !libs[0].Hidden || libs[1].Name != "Films" || !libs[1].Present.AdapterOnly {
		t.Fatalf("after edits = %+v, %v", libs, err)
	}
	if err := s.Rename(ctx, 1, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty rename = %v, want ErrInvalid", err)
	}
	if err := s.Rename(ctx, 99, "Nope"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("rename missing = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, 1); !errors.Is(err, ErrBuiltinLibrary) {
		t.Fatalf("delete builtin = %v, want ErrBuiltinLibrary", err)
	}
	filter, err := s.Create(ctx, "Short Lived", core.TypeShow, nil, "show", core.Presentations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, filter.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, filter.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("deleted filter still readable: %v", err)
	}
	// A deleted id is never reused.
	next, err := s.Create(ctx, "Next", core.TypeShow, nil, "show", core.Presentations{})
	if err != nil || next.ID == filter.ID {
		t.Fatalf("id reused: %+v, %v", next, err)
	}
}

func TestAllowsLibrary(t *testing.T) {
	owner := &core.User{IsOwner: true}
	open := &core.User{}
	restricted := &core.User{AllowedLibraries: []core.LibraryID{2}}
	if !owner.AllowsLibrary(7) || !open.AllowsLibrary(7) || !restricted.AllowsLibrary(2) || restricted.AllowsLibrary(1) {
		t.Fatal("library sharing semantics wrong (§1.4)")
	}
	var nobody *core.User
	if nobody.AllowsLibrary(1) {
		t.Fatal("nil user allowed a library")
	}
}

func libraryRules(f core.Filter) []core.LibraryExclusion {
	return []core.LibraryExclusion{{Action: "show", MediaType: string(f.Type), Match: "all", Rules: core.LibraryPredicateConditions(f)}}
}
