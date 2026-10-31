package tmdb

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Configuration fetches /configuration (cached for ImageURL too).
func (c *Client) Configuration(ctx context.Context) (*Configuration, error) {
	c.cfgMu.RLock()
	if c.img != nil {
		cfg := c.img
		c.cfgMu.RUnlock()
		return cfg, nil
	}
	c.cfgMu.RUnlock()
	var cfg Configuration
	if err := c.getJSON(ctx, "/configuration", url.Values{}, &cfg); err != nil {
		return nil, err
	}
	c.cfgMu.Lock()
	c.img = &cfg
	c.cfgMu.Unlock()
	return &cfg, nil
}

// SetConfiguration seeds the image configuration (tests / prefetch).
func (c *Client) SetConfiguration(cfg *Configuration) {
	c.cfgMu.Lock()
	c.img = cfg
	c.cfgMu.Unlock()
}

// ImageURL builds a full image URL. Implements core.ImageURLFunc when used as method value.
func (c *Client) ImageURL(path, size string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if size == "" {
		size = "original"
	}
	base := defaultImageBase
	c.cfgMu.RLock()
	if c.img != nil {
		if c.img.Images.SecureBaseURL != "" {
			base = c.img.Images.SecureBaseURL
		} else if c.img.Images.BaseURL != "" {
			base = c.img.Images.BaseURL
		}
	}
	c.cfgMu.RUnlock()
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + size + "/" + strings.TrimPrefix(path, "/")
}

// ImageURLFunc returns c.ImageURL as a core.ImageURLFunc.
func (c *Client) ImageURLFunc() core.ImageURLFunc { return c.ImageURL }

// Genres returns the genre list for "movie" or "tv".
func (c *Client) Genres(ctx context.Context, mediaType string) ([]Genre, error) {
	if mediaType != "movie" && mediaType != "tv" {
		return nil, fmt.Errorf("tmdb: genres: media type %q", mediaType)
	}
	var out GenreList
	err := c.getJSON(ctx, "/genre/"+mediaType+"/list", url.Values{}, &out)
	return out.Genres, err
}

// DiscoverParams controls discover queries. Zero values are omitted.
type DiscoverParams struct {
	Page                 int
	SortBy               string // e.g. popularity.desc, vote_count.desc, primary_release_date.desc
	VoteCountGTE         int
	VoteAverageGTE       float64
	WithGenres           string // comma/pipe list of genre ids
	Year                 int    // primary_release_year (movie) / first_air_date_year (tv)
	ReleaseDateGTE       string // YYYY-MM-DD
	ReleaseDateLTE       string
	FirstAirGTE          string
	FirstAirLTE          string
	IncludeAdult         bool
	WithOriginalLanguage string
	Region               string
	Extra                url.Values // merged last
}

func (p DiscoverParams) values(tv bool) url.Values {
	q := url.Values{}
	if p.SortBy != "" {
		q.Set("sort_by", p.SortBy)
	}
	if p.VoteCountGTE > 0 {
		q.Set("vote_count.gte", strconv.Itoa(p.VoteCountGTE))
	}
	if p.VoteAverageGTE > 0 {
		q.Set("vote_average.gte", strconv.FormatFloat(p.VoteAverageGTE, 'f', -1, 64))
	}
	if p.WithGenres != "" {
		q.Set("with_genres", p.WithGenres)
	}
	if p.IncludeAdult {
		q.Set("include_adult", "true")
	} else {
		q.Set("include_adult", "false")
	}
	if p.WithOriginalLanguage != "" {
		q.Set("with_original_language", p.WithOriginalLanguage)
	}
	if tv {
		if p.Year > 0 {
			q.Set("first_air_date_year", strconv.Itoa(p.Year))
		}
		if p.FirstAirGTE != "" {
			q.Set("first_air_date.gte", p.FirstAirGTE)
		}
		if p.FirstAirLTE != "" {
			q.Set("first_air_date.lte", p.FirstAirLTE)
		}
		if p.ReleaseDateGTE != "" && p.FirstAirGTE == "" {
			q.Set("first_air_date.gte", p.ReleaseDateGTE)
		}
		if p.ReleaseDateLTE != "" && p.FirstAirLTE == "" {
			q.Set("first_air_date.lte", p.ReleaseDateLTE)
		}
	} else {
		if p.Year > 0 {
			q.Set("primary_release_year", strconv.Itoa(p.Year))
		}
		if p.ReleaseDateGTE != "" {
			q.Set("primary_release_date.gte", p.ReleaseDateGTE)
		}
		if p.ReleaseDateLTE != "" {
			q.Set("primary_release_date.lte", p.ReleaseDateLTE)
		}
		if p.Region != "" {
			q.Set("region", p.Region)
		}
	}
	for k, vs := range p.Extra {
		q.Del(k)
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return pageQ(q, p.Page)
}

// DiscoverMovies pages /discover/movie.
func (c *Client) DiscoverMovies(ctx context.Context, p DiscoverParams) (*Paged[Movie], error) {
	var out Paged[Movie]
	err := c.getJSON(ctx, "/discover/movie", p.values(false), &out)
	return &out, err
}

// DiscoverTV pages /discover/tv.
func (c *Client) DiscoverTV(ctx context.Context, p DiscoverParams) (*Paged[TVShow], error) {
	var out Paged[TVShow]
	err := c.getJSON(ctx, "/discover/tv", p.values(true), &out)
	return &out, err
}

// MovieDetails fetches full movie details.
func (c *Client) MovieDetails(ctx context.Context, id int64) (*MovieDetails, error) {
	q := values("append_to_response", "credits,release_dates,external_ids,images,keywords,similar,recommendations")
	var out MovieDetails
	err := c.getJSON(ctx, fmt.Sprintf("/movie/%d", id), q, &out)
	return &out, err
}

// MovieReleaseDates fetches only a movie's release dates and certifications
// (the catalogue's rating re-selection; far lighter than MovieDetails).
func (c *Client) MovieReleaseDates(ctx context.Context, id int64) (*ReleaseDates, error) {
	var out ReleaseDates
	err := c.getJSON(ctx, fmt.Sprintf("/movie/%d/release_dates", id), nil, &out)
	return &out, err
}

// TVContentRatings fetches only a show's per-country content ratings.
func (c *Client) TVContentRatings(ctx context.Context, id int64) (*ContentRatings, error) {
	var out ContentRatings
	err := c.getJSON(ctx, fmt.Sprintf("/tv/%d/content_ratings", id), nil, &out)
	return &out, err
}

// TVDetails fetches full TV details.
func (c *Client) TVDetails(ctx context.Context, id int64) (*TVDetails, error) {
	q := values("append_to_response", "content_ratings,external_ids,credits,aggregate_credits,keywords,images,similar,recommendations")
	var out TVDetails
	err := c.getJSON(ctx, fmt.Sprintf("/tv/%d", id), q, &out)
	return &out, err
}

// SeasonDetails fetches one season with its episodes.
func (c *Client) SeasonDetails(ctx context.Context, tvID int64, season int) (*SeasonDetails, error) {
	var out SeasonDetails
	err := c.getJSON(ctx, fmt.Sprintf("/tv/%d/season/%d", tvID, season), url.Values{}, &out)
	return &out, err
}

// EpisodeDetails fetches one episode.
func (c *Client) EpisodeDetails(ctx context.Context, tvID int64, season, episode int) (*Episode, error) {
	var out Episode
	err := c.getJSON(ctx, fmt.Sprintf("/tv/%d/season/%d/episode/%d", tvID, season, episode), url.Values{}, &out)
	return &out, err
}

// EpisodeExternalIDs fetches external ids for one episode.
func (c *Client) EpisodeExternalIDs(ctx context.Context, tvID int64, season, episode int) (*ExternalIDs, error) {
	var out ExternalIDs
	err := c.getJSON(ctx, fmt.Sprintf("/tv/%d/season/%d/episode/%d/external_ids", tvID, season, episode), url.Values{}, &out)
	return &out, err
}

// CollectionDetails fetches a franchise collection.
func (c *Client) CollectionDetails(ctx context.Context, id int64) (*CollectionDetails, error) {
	var out CollectionDetails
	err := c.getJSON(ctx, fmt.Sprintf("/collection/%d", id), url.Values{}, &out)
	return &out, err
}

// Trending fetches /trending/{movie|tv|all}/{day|week}. For "all" results are MultiResult.
func (c *Client) TrendingMovies(ctx context.Context, window string, page int) (*Paged[Movie], error) {
	var out Paged[Movie]
	err := c.getJSON(ctx, "/trending/movie/"+validWindow(window), pageQ(nil, page), &out)
	return &out, err
}
func (c *Client) TrendingTV(ctx context.Context, window string, page int) (*Paged[TVShow], error) {
	var out Paged[TVShow]
	err := c.getJSON(ctx, "/trending/tv/"+validWindow(window), pageQ(nil, page), &out)
	return &out, err
}
func (c *Client) TrendingAll(ctx context.Context, window string, page int) (*Paged[MultiResult], error) {
	var out Paged[MultiResult]
	err := c.getJSON(ctx, "/trending/all/"+validWindow(window), pageQ(nil, page), &out)
	return &out, err
}
func validWindow(w string) string {
	if w == "day" {
		return "day"
	}
	return "week"
}

// MovieList fetches popular|top_rated|now_playing|upcoming.
func (c *Client) MovieList(ctx context.Context, list string, page int) (*Paged[Movie], error) {
	switch list {
	case "popular", "top_rated", "now_playing", "upcoming":
	default:
		return nil, fmt.Errorf("tmdb: unknown movie list %q", list)
	}
	q := pageQ(nil, page)
	if region := c.Region(); region != "" {
		q.Set("region", region)
	}
	var out Paged[Movie]
	err := c.getJSON(ctx, "/movie/"+list, q, &out)
	return &out, err
}

// TVList fetches popular|top_rated|airing_today|on_the_air.
func (c *Client) TVList(ctx context.Context, list string, page int) (*Paged[TVShow], error) {
	switch list {
	case "popular", "top_rated", "airing_today", "on_the_air":
	default:
		return nil, fmt.Errorf("tmdb: unknown tv list %q", list)
	}
	var out Paged[TVShow]
	err := c.getJSON(ctx, "/tv/"+list, pageQ(nil, page), &out)
	return &out, err
}

// SearchMovies / SearchTV / SearchMulti.
func (c *Client) SearchMovies(ctx context.Context, query string, page int) (*Paged[Movie], error) {
	q := pageQ(values("query", query, "include_adult", "false"), page)
	var out Paged[Movie]
	err := c.getJSON(ctx, "/search/movie", q, &out)
	return &out, err
}
func (c *Client) SearchTV(ctx context.Context, query string, page int) (*Paged[TVShow], error) {
	q := pageQ(values("query", query, "include_adult", "false"), page)
	var out Paged[TVShow]
	err := c.getJSON(ctx, "/search/tv", q, &out)
	return &out, err
}
func (c *Client) SearchMulti(ctx context.Context, query string, page int) (*Paged[MultiResult], error) {
	q := pageQ(values("query", query, "include_adult", "false"), page)
	var out Paged[MultiResult]
	err := c.getJSON(ctx, "/search/multi", q, &out)
	return &out, err
}

// FindByExternalID looks up by imdb ("tt...") or tvdb id.
// source accepts "imdb", "imdb_id", "tvdb", "tvdb_id".
func (c *Client) FindByExternalID(ctx context.Context, source, id string) (*FindResult, error) {
	src := map[string]string{"imdb": "imdb_id", "imdb_id": "imdb_id", "tvdb": "tvdb_id", "tvdb_id": "tvdb_id"}[strings.ToLower(source)]
	if src == "" {
		return nil, fmt.Errorf("tmdb: find: unsupported source %q", source)
	}
	var out FindResult
	err := c.getJSON(ctx, "/find/"+url.PathEscape(id), values("external_source", src), &out)
	return &out, err
}

// PersonDetails fetches /person/{id}.
func (c *Client) PersonDetails(ctx context.Context, id int64) (*Person, error) {
	var out Person
	err := c.getJSON(ctx, fmt.Sprintf("/person/%d", id), url.Values{}, &out)
	return &out, err
}

// PersonCombinedCredits fetches /person/{id}/combined_credits.
func (c *Client) PersonCombinedCredits(ctx context.Context, id int64) (*PersonCombinedCredits, error) {
	var out PersonCombinedCredits
	err := c.getJSON(ctx, fmt.Sprintf("/person/%d/combined_credits", id), url.Values{}, &out)
	return &out, err
}

// Similar / recommendations helpers.
func (c *Client) MovieSimilar(ctx context.Context, id int64, page int) (*Paged[Movie], error) {
	var out Paged[Movie]
	err := c.getJSON(ctx, fmt.Sprintf("/movie/%d/similar", id), pageQ(nil, page), &out)
	return &out, err
}
func (c *Client) MovieRecommendations(ctx context.Context, id int64, page int) (*Paged[Movie], error) {
	var out Paged[Movie]
	err := c.getJSON(ctx, fmt.Sprintf("/movie/%d/recommendations", id), pageQ(nil, page), &out)
	return &out, err
}
func (c *Client) TVSimilar(ctx context.Context, id int64, page int) (*Paged[TVShow], error) {
	var out Paged[TVShow]
	err := c.getJSON(ctx, fmt.Sprintf("/tv/%d/similar", id), pageQ(nil, page), &out)
	return &out, err
}
func (c *Client) TVRecommendations(ctx context.Context, id int64, page int) (*Paged[TVShow], error) {
	var out Paged[TVShow]
	err := c.getJSON(ctx, fmt.Sprintf("/tv/%d/recommendations", id), pageQ(nil, page), &out)
	return &out, err
}

// ParseDate parses a TMDB YYYY-MM-DD date (zero on empty/invalid).
func ParseDate(s string) time.Time {
	if len(s) < 10 {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return time.Time{}
	}
	return t
}

// GetJSON performs a rate-limited, cached GET of any TMDB v3 path (for
// example "/search/keyword") and decodes the JSON body into out. It lets
// callers reach endpoints this package has no typed helper for.
func (c *Client) GetJSON(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	} else {
		q = cloneValues(q)
	}
	return c.getJSON(ctx, path, q, out)
}

func cloneValues(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// MovieDetailsFresh and TVDetailsFresh bypass the response cache for callers
// that must verify current release dates. They never serve a stale response.
func (c *Client) MovieDetailsFresh(ctx context.Context, id int64) (*MovieDetails, error) {
	q := values("append_to_response", "credits,release_dates,external_ids,images,keywords,similar,recommendations")
	var out MovieDetails
	err := c.getFreshJSON(ctx, fmt.Sprintf("/movie/%d", id), q, &out)
	return &out, err
}

func (c *Client) TVDetailsFresh(ctx context.Context, id int64) (*TVDetails, error) {
	q := values("append_to_response", "content_ratings,external_ids,credits,aggregate_credits,keywords,images,similar,recommendations")
	var out TVDetails
	err := c.getFreshJSON(ctx, fmt.Sprintf("/tv/%d", id), q, &out)
	return &out, err
}
