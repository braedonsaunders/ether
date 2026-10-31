package catalog

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSearchYearFallbackAndLiteralTitles(t *testing.T) {
	c := newChunkTestCatalog(t)
	ctx := context.Background()
	titles := []core.Title{
		{Key: core.ShowKey(1), Type: core.TypeShow, Title: "Taskmaster", Year: 2015, Popularity: 1},
		{Key: core.ShowKey(2), Type: core.TypeShow, Title: "Taskmaster", Year: 2020, Popularity: 100},
		{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "Blade Runner 2049", Year: 2017},
		{Key: core.MovieKey(4), Type: core.TypeMovie, Title: "Blade Runner", Year: 1982, Popularity: 100},
		{Key: core.MovieKey(5), Type: core.TypeMovie, Title: "2012", Year: 2009},
		{Key: core.ShowKey(6), Type: core.TypeShow, Title: "Taskmaster", Year: 2030, Popularity: 1000},
	}
	for i := range titles {
		titles[i].Released = time.Date(titles[i].Year, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	c.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	if err := c.upsertMany(ctx, titles, upsertOpts{browsable: true}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		q    string
		want core.RatingKey
	}{
		{"Taskmaster 2015", core.ShowKey(1)}, {"Taskmaster (2015)", core.ShowKey(1)},
		{"Taskmaster 1999", core.ShowKey(2)}, {"Blade Runner 2049", core.MovieKey(3)}, {"2012", core.MovieKey(5)},
	} {
		t.Run(tt.q, func(t *testing.T) {
			got, err := c.Search(ctx, tt.q, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Key != tt.want {
				t.Fatalf("Search(%q) = %#v, want %v", tt.q, got, tt.want)
			}
		})
	}
	got, err := c.Search(ctx, "Taskmaster 2015", []core.MediaType{core.TypeMovie}, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("movie filter: %v %v", got, err)
	}
}

func TestSearchRanksDiacriticsAsEquivalent(t *testing.T) {
	items := []core.Title{{Title: "Shogun Warriors", Popularity: 100}, {Title: "Shōgun", Popularity: 1}}
	sortTitlesForSearch(items, "Shogun")
	if items[0].Title != "Shōgun" {
		t.Fatalf("exact accented title lost to %q", items[0].Title)
	}
}

// Embed the unused API methods; these searches must only call their matching
// search endpoint and must retain both the literal and stripped query.
type yearSearchTMDB struct {
	TMDB
	calls []string
}

func (f *yearSearchTMDB) SearchMovies(ctx context.Context, q string, page int) (*tmdb.Paged[tmdb.Movie], error) {
	f.calls = append(f.calls, q)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &tmdb.Paged[tmdb.Movie]{TotalPages: 1}
	if q == "Dune" {
		out.Results = []tmdb.Movie{{ID: 1, Title: "Dune", ReleaseDate: "1984-01-01", Popularity: 100}, {ID: 2, Title: "Dune", ReleaseDate: "2021-01-01", Popularity: 1}}
	}
	return out, nil
}
func (f *yearSearchTMDB) SearchTV(ctx context.Context, q string, page int) (*tmdb.Paged[tmdb.TVShow], error) {
	f.calls = append(f.calls, q)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &tmdb.Paged[tmdb.TVShow]{TotalPages: 1}
	if q == "Taskmaster" {
		out.Results = []tmdb.TVShow{{ID: 1, Name: "Taskmaster", FirstAirDate: "2020-01-01", Popularity: 100}, {ID: 2, Name: "Taskmaster", FirstAirDate: "2015-01-01", Popularity: 1}}
	}
	return out, nil
}
func (f *yearSearchTMDB) SearchMulti(ctx context.Context, q string, page int) (*tmdb.Paged[tmdb.MultiResult], error) {
	f.calls = append(f.calls, q)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &tmdb.Paged[tmdb.MultiResult]{TotalPages: 1}
	if q == "Dune" {
		out.Results = []tmdb.MultiResult{{ID: 1, MediaType: "movie", Title: "Dune", ReleaseDate: "1984-01-01", Popularity: 100}, {ID: 2, MediaType: "movie", Title: "Dune", ReleaseDate: "2021-01-01", Popularity: 1}}
	}
	return out, nil
}
func TestSearchYearRemoteFallback(t *testing.T) {
	for _, tt := range []struct {
		name, q, base string
		types         []core.MediaType
		year          int
	}{
		{"movie", "Dune 2021", "Dune", []core.MediaType{core.TypeMovie}, 2021},
		{"show", "Taskmaster (2015)", "Taskmaster", []core.MediaType{core.TypeShow}, 2015},
		{"multi", "Dune 2021", "Dune", nil, 2021},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newChunkTestCatalog(t)
			f := &yearSearchTMDB{}
			c.SetTMDB(f)
			got, err := c.Search(context.Background(), tt.q, tt.types, 1)
			if err != nil || len(got) != 1 || got[0].Year != tt.year {
				t.Fatalf("got %v, %v", got, err)
			}
			if !reflect.DeepEqual(f.calls, []string{tt.q, tt.base}) {
				t.Fatalf("queries %v", f.calls)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := c.Search(ctx, "Missing 2024", tt.types, 1); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

type failingSearchTMDB struct{ TMDB }

func (failingSearchTMDB) SearchMulti(context.Context, string, int) (*tmdb.Paged[tmdb.MultiResult], error) {
	return nil, errors.New("metadata down")
}
func (failingSearchTMDB) SearchMovies(context.Context, string, int) (*tmdb.Paged[tmdb.Movie], error) {
	return nil, errors.New("metadata down")
}
func (failingSearchTMDB) SearchTV(context.Context, string, int) (*tmdb.Paged[tmdb.TVShow], error) {
	return nil, errors.New("metadata down")
}

func TestSearchRemoteOutageKeepsLocalKeyword(t *testing.T) {
	c := newChunkTestCatalog(t)
	c.SetTMDB(failingSearchTMDB{})
	ctx := context.Background()
	released := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	episode := core.EpisodeKey(8, 1, 1)
	titles := []core.Title{
		{Key: core.MovieKey(9), Type: core.TypeMovie, Title: "Fixture Film", Released: released},
		{Key: episode, Type: core.TypeEpisode, ShowKey: core.ShowKey(8), Season: 1, Episode: 1, Title: "Pilot Unique", Released: released},
	}
	if err := c.upsertMany(ctx, titles, upsertOpts{browsable: true}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Search(ctx, "Fixture", []core.MediaType{core.TypeMovie, core.TypeShow, core.TypeEpisode}, 10)
	if err != nil || len(got) != 1 || got[0].Title != "Fixture Film" {
		t.Fatalf("movie: %#v %v", got, err)
	}
	got, err = c.Search(ctx, "Pilot Unique", []core.MediaType{core.TypeMovie, core.TypeShow, core.TypeEpisode}, 10)
	if err != nil || len(got) != 1 || got[0].Key != episode {
		t.Fatalf("episode: %#v %v", got, err)
	}
	got, err = c.Search(ctx, "zzzz-not-a-title", nil, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: %#v %v", got, err)
	}
}
