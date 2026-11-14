package frontend

import (
	"context"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// LiveTVProvider is the EPG provider identifier reported on Plex Live TV routes.
const LiveTVProvider = "tv.plex.providers.epg.xmltv:1"

// LiveGuide is an ordered owner guide descriptor. Its ID survives renames.
type LiveGuide struct {
	ID   string
	Name string
}

// LiveTVGuides optionally exposes owner guides without changing channel identity.
type LiveTVGuides interface {
	Guides(context.Context, *core.User) ([]LiveGuide, error)
	GuideChannels(context.Context, *core.User, string) ([]LiveTVChannel, error)
}

// LiveChannel contains public metadata, never provider URLs or credentials.
type LiveChannel struct {
	Guides                               []LiveGuide
	ID, Number, Name, Kind, Logo, Rating string
	Group                                string
	Favorite                             bool
	Images                               []Image
}
type LiveProgramme struct {
	Genres                          []string
	MediaType                       string
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

// LiveTVItem is the frontend boundary representation of a scheduled programme's item metadata.
type LiveTVItem struct {
	Key          RatingKey
	ShowKey      RatingKey
	Title        string
	EpisodeTitle string
	ShowTitle    string
	Summary      string
	Rating       string
	Thumb        string
	Art          string
	Genres       []string
	GUID         string
	ShowGUID     string
	MediaType    string
	Premiere     bool
	Live         bool
	Season       int
	Episode      int
	ReleasedMS   int64
}

// LiveTVChannel is the frontend boundary representation of a Live TV channel.
type LiveTVChannel struct {
	ID            string
	Number        string
	Name          string
	Kind          string
	Enabled       bool
	Logo          string
	LogoMatched   bool
	Rating        string
	SourceID      string
	GuideID       string
	GuideSourceID string
	Group         string
	Personal      bool
}

// LiveTVProgramme is the frontend boundary representation of a guide airing.
type LiveTVProgramme struct {
	ID        string
	ChannelID string
	Start     time.Time
	End       time.Time
	Lens      string
	Item      LiveTVItem
}

// LiveTVSessionInfo is the frontend boundary representation of an active Live TV tune session.
type LiveTVSessionInfo struct {
	ID             string
	SubscriptionID int64
	ActivityID     string
	MediaInfo      *MediaInfo
	Channel        LiveTVChannel
	Programme      *LiveTVProgramme
	StartedAt      time.Time
}

// LiveTVTuneDiagnosis is a structured classification of a Live TV tune or playback error.
type LiveTVTuneDiagnosis struct {
	Code           string
	Stage          string
	UpstreamStatus int
	RetryAfter     time.Duration
	Message        string
	LogAttrs       []any
}

// LiveTV applies the same per-viewer access and safety policy across all frontends.
// Viewer must be from the authenticated session. GUID resolution never returns
// channels or programmes that the viewer is not permitted to see.
type LiveTV interface {
	// Enabled reports whether any channels exist, independent of viewer access.
	Enabled() bool
	Channels(context.Context, *core.Viewer) ([]LiveChannel, error)
	Guide(context.Context, *core.Viewer, LiveGuideQuery) ([]LiveProgramme, error)
	ResolveChannelGUID(context.Context, *core.Viewer, string) (LiveChannel, error)
	ResolveProgrammeGUID(context.Context, *core.Viewer, string, LiveGuideQuery) (LiveProgramme, error)

	VisibleTo(ctx context.Context, u *User) bool
	ServeXMLTV(w http.ResponseWriter, r *http.Request, u *User)
	Stop(ctx context.Context, id string, u *User) error
	ServeDASHPart(w http.ResponseWriter, r *http.Request, id string, u *User, rep, n int, init bool)
	ServePlaylist(w http.ResponseWriter, r *http.Request, id string, u *User)
	ServeRelaySegment(w http.ResponseWriter, r *http.Request, id string, n int, u *User)
	ServeResource(w http.ResponseWriter, r *http.Request, id, resource string, u *User)
	AllowedChannels(ctx context.Context, u *User) ([]LiveTVChannel, error)
	AllowedChannel(ctx context.Context, u *User, id string) (LiveTVChannel, error)
	Channel(ctx context.Context, id string) (LiveTVChannel, error)
	ChannelForViewer(ctx context.Context, u *User, id string) (LiveTVChannel, error)
	AllChannels(ctx context.Context) ([]LiveTVChannel, error)
	ChannelPermitted(ctx context.Context, u *User, id string) (bool, error)
	LineupChangedFor(u *User) time.Time
	GuideForChannels(ctx context.Context, u *User, from, until time.Time, channelIDs []string) ([]LiveTVProgramme, error)
	GuideWindow(ctx context.Context, u *User, from, until time.Time) ([]LiveTVProgramme, error)
	GuideChannelLogo(ctx context.Context, c LiveTVChannel) string
	TuneWithSession(ctx context.Context, u *User, id string, caps ClientCaps, sessionID string) (LiveTVSessionInfo, error)
	Sessions(ctx context.Context, u *User) []LiveTVSessionInfo
	Session(ctx context.Context, id string, u *User) (LiveTVSessionInfo, error)
	ServeDASH(w http.ResponseWriter, r *http.Request, id string, u *User) error
	ServeHTTP(w http.ResponseWriter, r *http.Request, id string, u *User) error
	ViewingSessions() []*PlaySession
	NoteClientActivity()
	AuthenticateMediaGrant(r *http.Request, active func(context.Context, int64) *User) (*User, bool)
	LogoKey() []byte
}

var (
	diagnoseLiveTVFunc   func(stage string, err error) LiveTVTuneDiagnosis
	virtualDASHErrorFunc func(err error) (string, error, bool)
	showcaseLogoFunc     func(c LiveTVChannel) (string, []byte, bool)
)

// RegisterLiveTVHelpers wires host Live TV diagnostic and logo helpers for frontends.
func RegisterLiveTVHelpers(
	diagnose func(stage string, err error) LiveTVTuneDiagnosis,
	virtualDASH func(err error) (string, error, bool),
	showcaseLogo func(c LiveTVChannel) (string, []byte, bool),
) {
	diagnoseLiveTVFunc = diagnose
	virtualDASHErrorFunc = virtualDASH
	showcaseLogoFunc = showcaseLogo
}

// DiagnoseLiveTV classifies a Live TV tune or guide error.
func DiagnoseLiveTV(stage string, err error) LiveTVTuneDiagnosis {
	if diagnoseLiveTVFunc != nil {
		return diagnoseLiveTVFunc(stage, err)
	}
	if err != nil {
		return LiveTVTuneDiagnosis{Code: "livetv_unavailable", Stage: stage, Message: err.Error()}
	}
	return LiveTVTuneDiagnosis{Stage: stage}
}

// VirtualDASHError unpacks a virtual channel DASH error.
func VirtualDASHError(err error) (message string, cause error, ok bool) {
	if virtualDASHErrorFunc != nil {
		return virtualDASHErrorFunc(err)
	}
	return "", nil, false
}

// ShowcaseLogo returns the repository path and PNG bytes for a showcase channel.
func ShowcaseLogo(c LiveTVChannel) (repoPath string, png []byte, ok bool) {
	if showcaseLogoFunc != nil {
		return showcaseLogoFunc(c)
	}
	return "", nil, false
}
