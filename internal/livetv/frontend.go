package livetv

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func init() {
	frontend.RegisterLiveTVHelpers(
		func(stage string, err error) frontend.LiveTVTuneDiagnosis {
			d := Diagnose(stage, err)
			return frontend.LiveTVTuneDiagnosis{
				Code:           d.Code,
				Stage:          d.Stage,
				UpstreamStatus: d.UpstreamStatus,
				RetryAfter:     d.RetryAfter,
				Message:        d.Error(),
				LogAttrs:       d.LogAttrs(),
			}
		},
		func(err error) (string, error, bool) {
			var dashErr *VirtualDASHError
			if !errors.As(err, &dashErr) {
				return "", nil, false
			}
			return dashErr.Message, dashErr.Err, true
		},
		func(c frontend.LiveTVChannel) (string, []byte, bool) {
			return ShowcaseLogo(FromFrontendChannel(c))
		},
	)
}

// Frontend returns the frontend.LiveTV adapter for this service, or nil when s is nil.
func (s *Service) Frontend() frontend.LiveTV {
	if s == nil {
		return nil
	}
	return liveTVHost{s: s}
}

// ToFrontendChannel converts a Channel to the frontend boundary value type.
func ToFrontendChannel(c Channel) frontend.LiveTVChannel {
	return frontend.LiveTVChannel{
		ID:            c.ID,
		Number:        c.Number,
		Name:          c.Name,
		Kind:          c.Kind,
		Enabled:       c.Enabled,
		Logo:          c.Logo,
		LogoMatched:   c.LogoMatched,
		Rating:        c.Rating,
		SourceID:      c.SourceID,
		GuideID:       c.GuideID,
		GuideSourceID: c.GuideSourceID,
		Group:         c.Group,
		Personal:      c.Personal != nil,
	}
}

// FromFrontendChannel converts a frontend boundary channel to a Channel.
func FromFrontendChannel(c frontend.LiveTVChannel) Channel {
	ch := Channel{
		ID:            c.ID,
		Number:        c.Number,
		Name:          c.Name,
		Kind:          c.Kind,
		Enabled:       c.Enabled,
		Logo:          c.Logo,
		LogoMatched:   c.LogoMatched,
		Rating:        c.Rating,
		SourceID:      c.SourceID,
		GuideID:       c.GuideID,
		GuideSourceID: c.GuideSourceID,
		Group:         c.Group,
	}
	if c.Personal {
		ch.Personal = &PersonalRecipe{}
	}
	return ch
}

func toFrontendChannels(cs []Channel) []frontend.LiveTVChannel {
	out := make([]frontend.LiveTVChannel, len(cs))
	for i := range cs {
		out[i] = ToFrontendChannel(cs[i])
	}
	return out
}

func toFrontendItem(item Item) frontend.LiveTVItem {
	return frontend.LiveTVItem{
		Key:          item.Key,
		ShowKey:      item.ShowKey,
		Title:        item.Title,
		EpisodeTitle: item.EpisodeTitle,
		ShowTitle:    item.ShowTitle,
		Summary:      item.Summary,
		Rating:       item.Rating,
		Thumb:        item.Thumb,
		Art:          item.Art,
		Genres:       item.Genres,
		GUID:         item.GUID,
		ShowGUID:     item.ShowGUID,
		MediaType:    item.MediaType,
		Premiere:     item.Premiere,
		Live:         item.Live,
		Season:       item.Season,
		Episode:      item.Episode,
		ReleasedMS:   item.ReleasedMS,
	}
}

func toFrontendProgramme(p Programme) frontend.LiveTVProgramme {
	return frontend.LiveTVProgramme{
		ID:        p.ID,
		ChannelID: p.ChannelID,
		Start:     p.Start,
		End:       p.End,
		Lens:      p.Lens,
		Item:      toFrontendItem(p.Item),
	}
}

func toFrontendProgrammes(ps []Programme) []frontend.LiveTVProgramme {
	out := make([]frontend.LiveTVProgramme, len(ps))
	for i := range ps {
		out[i] = toFrontendProgramme(ps[i])
	}
	return out
}

func toFrontendSessionInfo(info SessionInfo) frontend.LiveTVSessionInfo {
	out := frontend.LiveTVSessionInfo{
		ID:             info.ID,
		SubscriptionID: info.SubscriptionID,
		ActivityID:     info.ActivityID,
		MediaInfo:      info.MediaInfo,
		Channel:        ToFrontendChannel(info.Channel),
		StartedAt:      info.StartedAt,
	}
	if info.Programme != nil {
		prog := toFrontendProgramme(*info.Programme)
		out.Programme = &prog
	}
	return out
}

type liveTVHost struct {
	s *Service
}

func (h liveTVHost) Enabled() bool { return h.s.Enabled() }

func (h liveTVHost) Channels(ctx context.Context, v *core.Viewer) ([]frontend.LiveChannel, error) {
	cs, err := h.s.AllowedChannels(ctx, v)
	if err != nil {
		return nil, err
	}
	out := make([]frontend.LiveChannel, len(cs))
	for i, c := range cs {
		out[i] = frontend.LiveChannel{ID: c.ID, Number: c.Number, Name: c.Name, Kind: c.Kind, Logo: c.Logo, Rating: c.Rating, Group: c.Group, Favorite: c.Favorite}
	}
	return out, nil
}

func (h liveTVHost) Guide(ctx context.Context, v *core.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
	var ps []Programme
	var err error
	if len(q.Channels) > 0 {
		ps, err = h.s.GuideForChannels(ctx, v, q.From, q.Until, q.Channels)
	} else {
		ps, err = h.s.Guide(ctx, v, q.From, q.Until)
	}
	if err != nil {
		return nil, err
	}
	out := make([]frontend.LiveProgramme, 0, len(ps))
	for _, p := range ps {
		out = append(out, frontend.LiveProgramme{ID: p.ID, ChannelID: p.ChannelID, Start: p.Start, End: p.End, Item: p.Item.Key, Title: p.Item.Title, Summary: p.Item.Summary, Rating: p.Item.Rating, ShowTitle: p.Item.ShowTitle, EpisodeTitle: p.Item.EpisodeTitle, Season: p.Item.Season, Episode: p.Item.Episode})
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

func (h liveTVHost) ResolveChannelGUID(context.Context, *core.Viewer, string) (frontend.LiveChannel, error) {
	return frontend.LiveChannel{}, core.ErrNotFound
}

func (h liveTVHost) ResolveProgrammeGUID(context.Context, *core.Viewer, string, frontend.LiveGuideQuery) (frontend.LiveProgramme, error) {
	return frontend.LiveProgramme{}, core.ErrNotFound
}

func (h liveTVHost) VisibleTo(ctx context.Context, u *frontend.User) bool {
	return h.s.VisibleTo(ctx, u)
}

func (h liveTVHost) ServeXMLTV(w http.ResponseWriter, r *http.Request, u *frontend.User) {
	h.s.ServeXMLTV(w, r, u)
}

func (h liveTVHost) Stop(ctx context.Context, id string, u *frontend.User) error {
	return h.s.Stop(ctx, id, u)
}

func (h liveTVHost) ServeDASHPart(w http.ResponseWriter, r *http.Request, id string, u *frontend.User, rep, n int, init bool) {
	h.s.ServeDASHPart(w, r, id, u, rep, n, init)
}

func (h liveTVHost) ServePlaylist(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) {
	h.s.ServePlaylist(w, r, id, u)
}

func (h liveTVHost) ServeRelaySegment(w http.ResponseWriter, r *http.Request, id string, n int, u *frontend.User) {
	h.s.ServeRelaySegment(w, r, id, n, u)
}

func (h liveTVHost) ServeResource(w http.ResponseWriter, r *http.Request, id, resource string, u *frontend.User) {
	h.s.ServeResource(w, r, id, resource, u)
}

func (h liveTVHost) AllowedChannels(ctx context.Context, u *frontend.User) ([]frontend.LiveTVChannel, error) {
	cs, err := h.s.AllowedChannels(ctx, u)
	if err != nil {
		return nil, err
	}
	return toFrontendChannels(cs), nil
}

func (h liveTVHost) AllowedChannel(ctx context.Context, u *frontend.User, id string) (frontend.LiveTVChannel, error) {
	c, err := h.s.AllowedChannel(ctx, u, id)
	if err != nil {
		return frontend.LiveTVChannel{}, err
	}
	return ToFrontendChannel(c), nil
}

func (h liveTVHost) Channel(ctx context.Context, id string) (frontend.LiveTVChannel, error) {
	if !h.s.Enabled() {
		return frontend.LiveTVChannel{}, core.ErrNotAvailable
	}
	c, err := h.s.Channel(ctx, id)
	if err != nil {
		return frontend.LiveTVChannel{}, err
	}
	return ToFrontendChannel(c), nil
}

func (h liveTVHost) ChannelForViewer(ctx context.Context, u *frontend.User, id string) (frontend.LiveTVChannel, error) {
	c, err := h.s.ChannelForViewer(ctx, u, id)
	if err != nil {
		return frontend.LiveTVChannel{}, err
	}
	return ToFrontendChannel(c), nil
}

func (h liveTVHost) AllChannels(ctx context.Context) ([]frontend.LiveTVChannel, error) {
	if !h.s.Enabled() {
		return nil, nil
	}
	cs, err := h.s.Channels(ctx)
	if err != nil {
		return nil, err
	}
	return toFrontendChannels(cs), nil
}

func (h liveTVHost) ChannelPermitted(ctx context.Context, u *frontend.User, id string) (bool, error) {
	return h.s.ChannelPermitted(ctx, u, id)
}

func (h liveTVHost) LineupChangedFor(u *frontend.User) time.Time {
	return h.s.LineupChangedFor(u)
}

func (h liveTVHost) GuideForChannels(ctx context.Context, u *frontend.User, from, until time.Time, channelIDs []string) ([]frontend.LiveTVProgramme, error) {
	ps, err := h.s.GuideForChannels(ctx, u, from, until, channelIDs)
	if err != nil {
		return nil, err
	}
	return toFrontendProgrammes(ps), nil
}

func (h liveTVHost) GuideWindow(ctx context.Context, u *frontend.User, from, until time.Time) ([]frontend.LiveTVProgramme, error) {
	ps, err := h.s.Guide(ctx, u, from, until)
	if err != nil {
		return nil, err
	}
	return toFrontendProgrammes(ps), nil
}

func (h liveTVHost) GuideChannelLogo(ctx context.Context, c frontend.LiveTVChannel) string {
	return h.s.GuideChannelLogo(ctx, FromFrontendChannel(c))
}

func (h liveTVHost) TuneWithSession(ctx context.Context, u *frontend.User, id string, caps frontend.ClientCaps, sessionID string) (frontend.LiveTVSessionInfo, error) {
	info, err := h.s.TuneWithSession(ctx, u, id, caps, sessionID)
	if err != nil {
		return frontend.LiveTVSessionInfo{}, err
	}
	return toFrontendSessionInfo(info), nil
}

func (h liveTVHost) Sessions(ctx context.Context, u *frontend.User) []frontend.LiveTVSessionInfo {
	infos := h.s.Sessions(ctx, u)
	out := make([]frontend.LiveTVSessionInfo, len(infos))
	for i := range infos {
		out[i] = toFrontendSessionInfo(infos[i])
	}
	return out
}

func (h liveTVHost) Session(ctx context.Context, id string, u *frontend.User) (frontend.LiveTVSessionInfo, error) {
	info, err := h.s.Session(ctx, id, u)
	if err != nil {
		return frontend.LiveTVSessionInfo{}, err
	}
	return toFrontendSessionInfo(info), nil
}

func (h liveTVHost) ServeDASH(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) error {
	return h.s.ServeDASH(w, r, id, u)
}

func (h liveTVHost) ServeHTTP(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) error {
	return h.s.ServeHTTP(w, r, id, u)
}

func (h liveTVHost) ViewingSessions() []*frontend.PlaySession {
	return h.s.ViewingSessions()
}

func (h liveTVHost) NoteClientActivity() {
	h.s.NoteClientActivity()
}

func (h liveTVHost) AuthenticateMediaGrant(r *http.Request, active func(context.Context, int64) *frontend.User) (*frontend.User, bool) {
	return h.s.AuthenticateMediaGrant(r, active)
}

func (h liveTVHost) LogoKey() []byte {
	return nil
}
