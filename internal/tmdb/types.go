package tmdb

// Paged is a TMDB paged list response.
type Paged[T any] struct {
	Page         int `json:"page"`
	Results      []T `json:"results"`
	TotalPages   int `json:"total_pages"`
	TotalResults int `json:"total_results"`
}

type Configuration struct {
	Images struct {
		BaseURL       string   `json:"base_url"`
		SecureBaseURL string   `json:"secure_base_url"`
		BackdropSizes []string `json:"backdrop_sizes"`
		LogoSizes     []string `json:"logo_sizes"`
		PosterSizes   []string `json:"poster_sizes"`
		ProfileSizes  []string `json:"profile_sizes"`
		StillSizes    []string `json:"still_sizes"`
	} `json:"images"`
	ChangeKeys []string `json:"change_keys"`
}

type Genre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type GenreList struct {
	Genres []Genre `json:"genres"`
}
type Company struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	LogoPath      string `json:"logo_path"`
	OriginCountry string `json:"origin_country"`
}
type Country struct {
	ISO3166 string `json:"iso_3166_1"`
	Name    string `json:"name"`
}
type Language struct {
	ISO639 string `json:"iso_639_1"`
	Name   string `json:"name"`
}
type ExternalIDs struct {
	ID          int64  `json:"id"`
	IMDBID      string `json:"imdb_id"`
	TVDBID      int64  `json:"tvdb_id"`
	FacebookID  string `json:"facebook_id"`
	InstagramID string `json:"instagram_id"`
	TwitterID   string `json:"twitter_id"`
	WikidataID  string `json:"wikidata_id"`
}
type CastMember struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Character   string `json:"character"`
	Job         string `json:"job"`
	Department  string `json:"department"`
	Order       int    `json:"order"`
	ProfilePath string `json:"profile_path"`
	// aggregate credits
	Roles []struct {
		Character string `json:"character"`
	} `json:"roles"`
	Jobs []struct {
		Job string `json:"job"`
	} `json:"jobs"`
}
type Credits struct {
	Cast []CastMember `json:"cast"`
	Crew []CastMember `json:"crew"`
}
type Keyword struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type Keywords struct {
	Keywords []Keyword `json:"keywords"`
	Results  []Keyword `json:"results"` // tv uses results
}
type ImageFile struct {
	FilePath string  `json:"file_path"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	ISO639   string  `json:"iso_639_1"`
	VoteAvg  float64 `json:"vote_average"`
}
type Images struct {
	Backdrops []ImageFile `json:"backdrops"`
	Posters   []ImageFile `json:"posters"`
	Logos     []ImageFile `json:"logos"`
	Still     []ImageFile `json:"stills"`
}

// Movie is a list/search/discover movie item.
type Movie struct {
	ID               int64   `json:"id"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"original_title"`
	Overview         string  `json:"overview"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	ReleaseDate      string  `json:"release_date"`
	GenreIDs         []int   `json:"genre_ids"`
	Genres           []Genre `json:"genres"`
	OriginalLanguage string  `json:"original_language"`
	Popularity       float64 `json:"popularity"`
	VoteAverage      float64 `json:"vote_average"`
	VoteCount        int     `json:"vote_count"`
	Adult            bool    `json:"adult"`
	MediaType        string  `json:"media_type"`
}

type ReleaseDateEntry struct {
	Certification string `json:"certification"`
	ReleaseDate   string `json:"release_date"`
	Type          int    `json:"type"`
	Note          string `json:"note"`
}
type ReleaseDates struct {
	Results []struct {
		ISO3166     string             `json:"iso_3166_1"`
		ReleaseDate []ReleaseDateEntry `json:"release_dates"`
	} `json:"results"`
}
type CollectionRef struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
}

// AltTitles is the alternative_titles appendix: other names a title goes by
// (romanized originals such as "Sousou no Frieren", regional and working
// titles). Movies list them under titles, TV under results.
type AltTitles struct {
	Titles  []AltTitle `json:"titles"`
	Results []AltTitle `json:"results"`
}

// AltTitle is one alternative title.
type AltTitle struct {
	Country string `json:"iso_3166_1"`
	Title   string `json:"title"`
	Type    string `json:"type"`
}

// MovieDetails is /movie/{id} with append_to_response.
type MovieDetails struct {
	Movie
	Tagline           string         `json:"tagline"`
	Runtime           int            `json:"runtime"`
	Status            string         `json:"status"`
	IMDBID            string         `json:"imdb_id"`
	Budget            int64          `json:"budget"`
	Revenue           int64          `json:"revenue"`
	Homepage          string         `json:"homepage"`
	BelongsTo         *CollectionRef `json:"belongs_to_collection"`
	ProductionCompany []Company      `json:"production_companies"`
	ProductionCountry []Country      `json:"production_countries"`
	SpokenLanguages   []Language     `json:"spoken_languages"`
	Credits           *Credits       `json:"credits"`
	ReleaseDates      *ReleaseDates  `json:"release_dates"`
	ExternalIDs       *ExternalIDs   `json:"external_ids"`
	Images            *Images        `json:"images"`
	Keywords          *Keywords      `json:"keywords"`
	AlternativeTitles *AltTitles     `json:"alternative_titles"`
	Similar           *Paged[Movie]  `json:"similar"`
	Recommendations   *Paged[Movie]  `json:"recommendations"`
}

// TVShow is a list/search/discover TV item.
type TVShow struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name"`
	OriginalName     string   `json:"original_name"`
	Overview         string   `json:"overview"`
	PosterPath       string   `json:"poster_path"`
	BackdropPath     string   `json:"backdrop_path"`
	FirstAirDate     string   `json:"first_air_date"`
	GenreIDs         []int    `json:"genre_ids"`
	Genres           []Genre  `json:"genres"`
	OriginCountry    []string `json:"origin_country"`
	OriginalLanguage string   `json:"original_language"`
	Popularity       float64  `json:"popularity"`
	VoteAverage      float64  `json:"vote_average"`
	VoteCount        int      `json:"vote_count"`
	Adult            bool     `json:"adult"`
	MediaType        string   `json:"media_type"`
}

type SeasonSummary struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Overview     string  `json:"overview"`
	SeasonNumber int     `json:"season_number"`
	EpisodeCount int     `json:"episode_count"`
	AirDate      string  `json:"air_date"`
	PosterPath   string  `json:"poster_path"`
	VoteAverage  float64 `json:"vote_average"`
}
type Network struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type ContentRatings struct {
	Results []struct {
		ISO3166 string `json:"iso_3166_1"`
		Rating  string `json:"rating"`
	} `json:"results"`
}

// TVDetails is /tv/{id} with append_to_response.
type TVDetails struct {
	TVShow
	Tagline           string          `json:"tagline"`
	Status            string          `json:"status"`
	Type              string          `json:"type"`
	EpisodeRunTime    []int           `json:"episode_run_time"`
	NumberOfSeasons   int             `json:"number_of_seasons"`
	NumberOfEpisodes  int             `json:"number_of_episodes"`
	LastAirDate       string          `json:"last_air_date"`
	LastEpisodeToAir  *Episode        `json:"last_episode_to_air"`
	InProduction      bool            `json:"in_production"`
	Homepage          string          `json:"homepage"`
	Seasons           []SeasonSummary `json:"seasons"`
	Networks          []Network       `json:"networks"`
	ProductionCompany []Company       `json:"production_companies"`
	ProductionCountry []Country       `json:"production_countries"`
	CreatedBy         []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		ProfilePath string `json:"profile_path"`
	} `json:"created_by"`
	Credits           *Credits        `json:"credits"`
	AggregateCredits  *Credits        `json:"aggregate_credits"`
	ContentRatings    *ContentRatings `json:"content_ratings"`
	ExternalIDs       *ExternalIDs    `json:"external_ids"`
	Images            *Images         `json:"images"`
	Keywords          *Keywords       `json:"keywords"`
	AlternativeTitles *AltTitles      `json:"alternative_titles"`
	Similar           *Paged[TVShow]  `json:"similar"`
	Recommendations   *Paged[TVShow]  `json:"recommendations"`
}

// Episode is a TV episode.
type Episode struct {
	ID            int64        `json:"id"`
	Name          string       `json:"name"`
	Overview      string       `json:"overview"`
	EpisodeNumber int          `json:"episode_number"`
	SeasonNumber  int          `json:"season_number"`
	AirDate       string       `json:"air_date"`
	Runtime       *int         `json:"runtime"`
	StillPath     string       `json:"still_path"`
	VoteAverage   float64      `json:"vote_average"`
	VoteCount     int          `json:"vote_count"`
	ShowID        int64        `json:"show_id"`
	Crew          []CastMember `json:"crew"`
	GuestStars    []CastMember `json:"guest_stars"`
}

// SeasonDetails is /tv/{id}/season/{n}.
type SeasonDetails struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Overview     string    `json:"overview"`
	SeasonNumber int       `json:"season_number"`
	AirDate      string    `json:"air_date"`
	PosterPath   string    `json:"poster_path"`
	Episodes     []Episode `json:"episodes"`
	VoteAverage  float64   `json:"vote_average"`
}

// CollectionDetails is /collection/{id}.
type CollectionDetails struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Overview     string  `json:"overview"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	Parts        []Movie `json:"parts"`
}

// MultiResult is one /search/multi item (movie, tv or person fields unioned).
type MultiResult struct {
	ID               int64   `json:"id"`
	MediaType        string  `json:"media_type"`
	Title            string  `json:"title"`
	Name             string  `json:"name"`
	OriginalTitle    string  `json:"original_title"`
	OriginalName     string  `json:"original_name"`
	Overview         string  `json:"overview"`
	PosterPath       string  `json:"poster_path"`
	BackdropPath     string  `json:"backdrop_path"`
	ProfilePath      string  `json:"profile_path"`
	ReleaseDate      string  `json:"release_date"`
	FirstAirDate     string  `json:"first_air_date"`
	GenreIDs         []int   `json:"genre_ids"`
	OriginalLanguage string  `json:"original_language"`
	Popularity       float64 `json:"popularity"`
	VoteAverage      float64 `json:"vote_average"`
	VoteCount        int     `json:"vote_count"`
}

func (m MultiResult) AsMovie() Movie {
	return Movie{ID: m.ID, Title: firstNonEmpty(m.Title, m.Name), OriginalTitle: firstNonEmpty(m.OriginalTitle, m.OriginalName),
		Overview: m.Overview, PosterPath: m.PosterPath, BackdropPath: m.BackdropPath, ReleaseDate: m.ReleaseDate,
		GenreIDs: m.GenreIDs, OriginalLanguage: m.OriginalLanguage, Popularity: m.Popularity, VoteAverage: m.VoteAverage, VoteCount: m.VoteCount, MediaType: m.MediaType}
}
func (m MultiResult) AsTV() TVShow {
	return TVShow{ID: m.ID, Name: firstNonEmpty(m.Name, m.Title), OriginalName: firstNonEmpty(m.OriginalName, m.OriginalTitle),
		Overview: m.Overview, PosterPath: m.PosterPath, BackdropPath: m.BackdropPath, FirstAirDate: m.FirstAirDate,
		GenreIDs: m.GenreIDs, OriginalLanguage: m.OriginalLanguage, Popularity: m.Popularity, VoteAverage: m.VoteAverage, VoteCount: m.VoteCount, MediaType: m.MediaType}
}
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// FindResult is /find/{id}.
type FindResult struct {
	MovieResults     []Movie         `json:"movie_results"`
	TVResults        []TVShow        `json:"tv_results"`
	TVEpisodeResults []Episode       `json:"tv_episode_results"`
	TVSeasonResults  []SeasonSummary `json:"tv_season_results"`
	PersonResults    []Person        `json:"person_results"`
}

type Person struct {
	ID                 int64   `json:"id"`
	Name               string  `json:"name"`
	Biography          string  `json:"biography"`
	Birthday           string  `json:"birthday"`
	Deathday           string  `json:"deathday"`
	PlaceOfBirth       string  `json:"place_of_birth"`
	ProfilePath        string  `json:"profile_path"`
	KnownForDepartment string  `json:"known_for_department"`
	Popularity         float64 `json:"popularity"`
	IMDBID             string  `json:"imdb_id"`
}

// PersonCredit is one work in /person/{id}/combined_credits.
type PersonCredit struct {
	MultiResult
	Character    string `json:"character"`
	Job          string `json:"job"`
	Department   string `json:"department"`
	EpisodeCount int    `json:"episode_count"`
}

// PersonCombinedCredits is /person/{id}/combined_credits.
type PersonCombinedCredits struct {
	ID   int64          `json:"id"`
	Cast []PersonCredit `json:"cast"`
	Crew []PersonCredit `json:"crew"`
}
