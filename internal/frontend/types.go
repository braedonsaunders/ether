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
type MediaType = core.MediaType
type Page = core.Page
type SortOrder = core.SortOrder
type WatchStatus = core.WatchStatus
type StreamInfo = core.StreamInfo
type MediaInfo = core.MediaInfo
type DecideOptions = core.DecideOptions
type ClientCaps = core.ClientCaps
type PlaybackSourceHistory = core.PlaybackSourceHistory
type Decision = core.Decision
type HLSSession = core.HLSSession

// Catalogue key kinds and constructors stay owned by core. Adapters compare
// and rebuild parent keys through these aliases.
const (
	KindShow    = core.KindShow
	KindSeason  = core.KindSeason
	KindEpisode = core.KindEpisode
)

func ShowKey(tmdb int64) RatingKey { return core.ShowKey(tmdb) }
func SeasonKey(show int64, season int) RatingKey {
	return core.SeasonKey(show, season)
}
