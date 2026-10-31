// Package core holds the domain types and the interfaces between Ether's
// components. It has no dependencies on other Ether packages, so every
// component can import it. Changing a type here is a contract change: keep
// edits additive.
package core

import (
	"errors"
	"time"
)

// MediaType matches Plex's metadata "type" attribute.
type MediaType string

const (
	TypeMovie   MediaType = "movie"
	TypeShow    MediaType = "show"
	TypeSeason  MediaType = "season"
	TypeEpisode MediaType = "episode"
)

// IDs are the external identifiers of a title. Episodes carry their show's
// IMDb/TVDB ids in ShowIDs on Title, and their own TMDB episode id here.
type IDs struct {
	TMDB int64  `json:"tmdb,omitempty"`
	IMDB string `json:"imdb,omitempty"`
	TVDB int64  `json:"tvdb,omitempty"`
}

type Person struct {
	TMDB      int64  `json:"tmdb,omitempty"`
	IMDB      string `json:"imdb,omitempty"`
	ID        string `json:"id,omitempty"` // catalogue person id when there is no TMDB id
	Name      string `json:"name"`
	Role      string `json:"role,omitempty"` // character for cast, job for crew
	ThumbPath string `json:"thumbPath,omitempty"`
}

// Title is a catalogue item: movie, show, season or episode. Image paths are
// TMDB paths ("/abc.jpg"); the front end turns them into URLs.
type Title struct {
	Key  RatingKey `json:"key"`
	Type MediaType `json:"type"`
	IDs  IDs       `json:"ids"`

	// Seasons and episodes.
	ShowKey           RatingKey `json:"showKey,omitempty"`
	ShowIDs           IDs       `json:"showIds,omitempty"`
	ShowTitle         string    `json:"showTitle,omitempty"`
	ShowOriginalTitle string    `json:"showOriginalTitle,omitempty"` // canonical original series name, distinct from episode title
	ShowYear          int       `json:"showYear,omitempty"`          // series premiere year, distinct from episode air year
	SeasonTitle       string    `json:"seasonTitle,omitempty"`
	Season            int       `json:"season,omitempty"`
	Episode           int       `json:"episode,omitempty"`
	// AbsoluteEpisode counts an episode from the series start across its
	// regular seasons; 0 when an earlier season's size is unknown (additive).
	AbsoluteEpisode int `json:"absoluteEpisode,omitempty"`

	Title                       string        `json:"title"`
	OriginalTitle               string        `json:"originalTitle,omitempty"`
	SortTitle                   string        `json:"sortTitle,omitempty"`
	Tagline                     string        `json:"tagline,omitempty"`
	Summary                     string        `json:"summary,omitempty"`
	Year                        int           `json:"year,omitempty"`
	Released                    time.Time     `json:"released,omitempty"`   // first air / release date
	AddedAt                     time.Time     `json:"addedAt,omitzero"`     // series/season latest aired episode, or a derived air date; not a catalogue insertion time
	LastAirDate                 time.Time     `json:"lastAirDate,omitzero"` // last aired episode date when known
	HomeReleaseChecked          bool          `json:"homeReleaseChecked,omitempty"`
	HomeReleased                time.Time     `json:"homeReleased,omitempty"`
	HomeReleaseFallbackAdmitted bool          `json:"homeReleaseFallbackAdmitted,omitempty"`
	Runtime                     time.Duration `json:"runtime,omitempty"`
	Status                      string        `json:"status,omitempty"` // TMDB status: Released, In Production, Ended...

	Genres            []string `json:"genres,omitempty"`
	Keywords          []string `json:"keywords,omitempty"` // TMDB keywords (lower-case), for content-based recommendations
	RemoteTrailerName string   `json:"remoteTrailerName,omitempty"`
	RemoteTrailerURL  string   `json:"remoteTrailerURL,omitempty"`
	Studios           []string `json:"studios,omitempty"`
	Countries         []string `json:"countries,omitempty"`
	Language          string   `json:"language,omitempty"` // ISO 639-1 original language
	ContentRating     string   `json:"contentRating,omitempty"`
	Adult             bool     `json:"adult,omitempty"`

	Rating     float64 `json:"rating,omitempty"` // TMDB vote average 0-10
	VoteCount  int     `json:"voteCount,omitempty"`
	Popularity float64 `json:"popularity,omitempty"`

	PosterPath   string `json:"posterPath,omitempty"`
	BackdropPath string `json:"backdropPath,omitempty"`
	StillPath    string `json:"stillPath,omitempty"` // episodes
	LogoPath     string `json:"logoPath,omitempty"`

	Cast      []Person `json:"cast,omitempty"`
	Directors []Person `json:"directors,omitempty"`
	Writers   []Person `json:"writers,omitempty"`
	Crew      []Person `json:"crew,omitempty"` // creators, cinematographers, composers (additive)

	CollectionID   int64  `json:"collectionId,omitempty"` // TMDB franchise
	CollectionName string `json:"collectionName,omitempty"`

	ChildCount int `json:"childCount,omitempty"` // seasons of a show, episodes of a season
	LeafCount  int `json:"leafCount,omitempty"`  // episodes of a show

	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// ShowTrajectory summarizes a series' season/episode rating arc (additive).
type ShowTrajectory struct {
	ShowKey       RatingKey `json:"showKey"`
	EarlyRating   float64   `json:"earlyRating,omitempty"`
	FinaleRating  float64   `json:"finaleRating,omitempty"`
	PeakRating    float64   `json:"peakRating,omitempty"`
	RatedSeasons  int       `json:"ratedSeasons,omitempty"`
	SticksLanding bool      `json:"sticksLanding,omitempty"`
}

// Released reports whether the title has a release/air date in the past.
func (t *Title) IsReleased(now time.Time) bool {
	return !t.Released.IsZero() && !t.Released.After(now)
}

var (
	ErrNotFound          = errors.New("not found")
	ErrNotAvailable      = errors.New("not available yet") // no acceptable release exists
	ErrUnreleased        = errors.New("title has not been released")
	ErrArticleMissing    = errors.New("article missing on all providers")
	ErrArticleCorrupt    = errors.New("corrupt article or media data")
	ErrReleaseRejected   = errors.New("release cannot be streamed")
	ErrSourceUnavailable = errors.New("media source temporarily unavailable")
	ErrBudgetExhausted   = errors.New("indexer daily budget exhausted")
	ErrStreamLimit       = errors.New("stream limit reached")
	ErrUnauthorized      = errors.New("unauthorized")
	// ErrPolicyUnconfirmed means the durable policy store could not be read.
	// Callers may keep an unrestricted cached decision. Restricted viewers fail closed.
	ErrPolicyUnconfirmed = errors.New("policy store unavailable")
)

// User is a Plex account with access to this server: the owner or a friend
// the owner shared with on plex.tv.
type User struct {
	// Timestamps are internal identity metadata and do not change existing wire JSON.
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
	// Delegated marks short-lived server credentials; these never grant owner access.
	Delegated bool   `json:"-"`
	ID        int64  `json:"id"` // local id
	PlexID    int64  `json:"plexId"`
	UUID      string `json:"uuid,omitempty"`
	Username  string `json:"username"`
	Title     string `json:"title"` // display name
	Email     string `json:"email,omitempty"`
	Thumb     string `json:"thumb,omitempty"`
	IsOwner   bool   `json:"isOwner"`

	StreamLimit       int      `json:"streamLimit"`       // 0 = server default
	MaxContentRating  string   `json:"maxContentRating"`  // "" = unrestricted, e.g. "PG-13", "TV-14"
	SubtitleLanguages []string `json:"subtitleLanguages"` // ISO 639-1, preference order
	AudioLanguages    []string `json:"audioLanguages"`
	AllowedSections   []int    `json:"allowedSections"` // empty = all
}

// ClientCaps is what a Plex client can play, derived from X-Plex-* headers,
// the client profile and its declared limits.
type ClientCaps struct {
	Profiles        *ClientCapabilityProfiles `json:"profiles,omitempty"`
	Protocol        Protocol                  `json:"protocol,omitempty"`
	Product         string                    `json:"product"`  // X-Plex-Product: "Plex Web", "Plex for iOS", "Plex for Apple TV"
	Platform        string                    `json:"platform"` // X-Plex-Platform
	PlatformVersion string                    `json:"platformVersion"`
	Device          string                    `json:"device"`
	DeviceName      string                    `json:"deviceName"`
	ClientID        string                    `json:"clientId"` // X-Plex-Client-Identifier
	Version         string                    `json:"version"`

	MaxHeight      int                 `json:"maxHeight"` // 2160, 1080, 720...
	HDR10          bool                `json:"hdr10"`
	DolbyVision    bool                `json:"dolbyVision"`
	VideoCodecs    []string            `json:"videoCodecs"`              // h264, hevc, av1, vp9
	AudioCodecs    []string            `json:"audioCodecs"`              // aac, ac3, eac3, truehd, dts, opus, flac
	Containers     []string            `json:"containers"`               // mp4, mkv, mpegts
	HLSContainers  []string            `json:"hlsContainers,omitempty"`  // explicit segmented output targets; distinct from direct-play containers
	MaxBitrateKbps int                 `json:"maxBitrateKbps"`           // 0 = unlimited
	SubtitleCodecs []string            `json:"subtitleCodecs,omitempty"` // native image/text subtitle rendering
	DVProfiles     []int               `json:"dvProfiles,omitempty"`     // explicit Dolby Vision profiles; empty uses product defaults
	VideoProfiles  map[string][]string `json:"videoProfiles,omitempty"`
	VideoLevels    map[string]int      `json:"videoLevels,omitempty"` // codec level in ffprobe units (H.264 51 = 5.1)
	Remote         bool                `json:"remote"`                // not on the server's LAN

	// TranscodeAudioCodecs is the explicit audio target for the current
	// streaming protocol. It constrains remux/transcode output only; source
	// selection and direct play continue to use AudioCodecs.
	TranscodeAudioCodecs []string `json:"transcodeAudioCodecs,omitempty"`

	// Download marks a request for an offline download (Plex Downloads /
	// Sync) rather than live playback: the resolver prefers compact,
	// mobile-native releases and the whole file is remuxed (additive).
	Download bool `json:"download,omitempty"`
}

func (c ClientCaps) SupportsVideo(codec string) bool  { return contains(c.VideoCodecs, codec) }
func (c ClientCaps) SupportsAudio(codec string) bool  { return contains(c.AudioCodecs, codec) }
func (c ClientCaps) SupportsContainer(ct string) bool { return contains(c.Containers, ct) }

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
