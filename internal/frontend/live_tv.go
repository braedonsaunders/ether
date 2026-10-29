package frontend

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

// LiveChannel contains public metadata and a neutral channel identifier.
// It never contains IPTV source URLs or credentials.
type LiveChannel struct {
	ID, Number, Name, Kind, Logo, Rating string
	Favorite                             bool
	Images                               []Image
}
type LiveProgramme struct {
	ID, ChannelID                   string
	Start, End                      time.Time
	Item                            core.RatingKey
	Title, Summary, Rating, Artwork string
	ShowTitle, EpisodeTitle         string
	Season, Episode                 int
	Images                          []Image
}
type LiveGuideQuery struct {
	Channels    []string
	From, Until time.Time
	Limit       int
	Device      core.Device
}

// LiveTV applies the same per-viewer access and safety policy as Plex Live TV.
// Viewer must be from the authenticated session. GUID resolution never returns
// channels or programmes that the viewer is not permitted to see.
type LiveTV interface {
	Channels(context.Context, *core.Viewer) ([]LiveChannel, error)
	Guide(context.Context, *core.Viewer, LiveGuideQuery) ([]LiveProgramme, error)
	ResolveChannelGUID(context.Context, *core.Viewer, string) (LiveChannel, error)
	ResolveProgrammeGUID(context.Context, *core.Viewer, string, LiveGuideQuery) (LiveProgramme, error)
}
