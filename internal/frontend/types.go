package frontend

import "github.com/braedonsaunders/ether/internal/core"

// Protocol adapters use these aliases to exchange host values without importing
// domain packages directly. Their implementations remain owned by core.
type Viewer = core.Viewer
type Device = core.Device
type Title = core.Title
type Collection = core.Collection
type Person = core.Person
type RatingKey = core.RatingKey

// ItemRef is a plugin item's stable identity (spec §2.2). Protocols that
// address plugin items by their ItemRef verbatim (Stremio's "pk:" id) exchange
// it through the host seam rather than importing core.
type ItemRef = core.ItemRef
type Playlist = core.Playlist
type MediaType = core.MediaType
type Page = core.Page
type WatchStatus = core.WatchStatus
type StreamInfo = core.StreamInfo
type MediaInfo = core.MediaInfo
type DecideOptions = core.DecideOptions
type ClientCaps = core.ClientCaps
type PlaybackSourceHistory = core.PlaybackSourceHistory
type Decision = core.Decision
type FacetValue = core.FacetValue
type User = core.User

// LibraryID names one library (built-ins 1 = Movies, 2 = TV Shows).
type LibraryID = core.LibraryID

// The built-in libraries.
const (
	LibraryMovies = core.LibraryMovies
	LibraryShows  = core.LibraryShows
)

// Home hub values stay owned by core. Adapters render hubs through these
// aliases.
type Hub = core.Hub
type HubContext = core.HubContext
type HomeSituation = core.HomeSituation

// Playback session and decision values stay owned by core. Adapters
// observe and render them through these aliases.
type PlaySession = core.PlaySession
type DecisionKind = core.DecisionKind
type DecisionCandidate = core.DecisionCandidate
type DecisionTrace = core.DecisionTrace
type ReleaseVariant = core.ReleaseVariant

const (
	DecisionDirectPlay = core.DecisionDirectPlay
	DecisionRemux      = core.DecisionRemux
	DecisionTranscode  = core.DecisionTranscode
)

func IsSurroundAudioCodec(c string) bool { return core.IsSurroundAudioCodec(c) }

// Release and variant-resolution values stay owned by core. Adapters
// describe and label versions through these aliases.
type Source = core.Source
type Release = core.Release
type ReleaseInfo = core.ReleaseInfo
type ResolveRequest = core.ResolveRequest

const MaxVariants = core.MaxVariants

func ResolutionTier(w, h int) int { return core.ResolutionTier(w, h) }

func SourceLabel(s Source) string { return core.SourceLabel(s) }

// Library browse and history values stay owned by core. Adapters build
// queries and read results through these aliases.
type BrowseQuery = core.BrowseQuery
type CreditFilter = core.CreditFilter
type Filter = core.Filter
type FilterGroup = core.FilterGroup
type StringFilter = core.StringFilter
type SortOrder = core.SortOrder
type SortSpec = core.SortSpec
type HistoryEntry = core.HistoryEntry
type HistoryQuery = core.HistoryQuery
type HistoryCount = core.HistoryCount
type WatchPage = core.WatchPage
type TitleFacts = core.TitleFacts

const (
	SortAdded      = core.SortAdded
	SortDuration   = core.SortDuration
	SortPopularity = core.SortPopularity
	SortPlayed     = core.SortPlayed
	SortRating     = core.SortRating
	SortReleased   = core.SortReleased
	SortTitle      = core.SortTitle
	SortUpdated    = core.SortUpdated
	SortYear       = core.SortYear
)

// Catalogue key kinds and constructors stay owned by core. Adapters compare
// and rebuild parent keys through these aliases.
const (
	KindShow    = core.KindShow
	KindSeason  = core.KindSeason
	KindEpisode = core.KindEpisode
	KindExtra   = core.KindExtra
)

func ShowKey(tmdb int64) RatingKey { return core.ShowKey(tmdb) }

func SeasonKey(show int64, season int) RatingKey {
	return core.SeasonKey(show, season)
}

// Protocol integration contracts remain implemented by the host.
type Presentations = core.Presentations
type SessionPolicy = core.SessionPolicy
type FrontendHostPolicy = core.FrontendHostPolicy
type DeliveryPolicy = core.DeliveryPolicy
type RangeResponse = core.RangeResponse
type RangeRepresentation = core.RangeRepresentation

var WithRangeResponse = core.WithRangeResponse
var IsGrantCredential = core.IsGrantCredential
