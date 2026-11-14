package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/livetv"
)

const maxLiveProgrammes = 10000

var errLiveQuery = errors.New("invalid live guide query")

type surfaceLiveTV struct {
	app              *App
	surface          string // the app this lineup is for; the owner's per-app visibility applies
	channelArtwork   func(*core.Viewer, livetv.Channel) frontend.Image
	programmeArtwork func(*core.Viewer, livetv.Programme) []frontend.Image
	artwork          frontend.Artwork
}

func (s surfaceLiveTV) Enabled() bool {
	return s.app != nil && s.app.livetv != nil && s.app.livetv.Enabled()
}

func (s surfaceLiveTV) host() frontend.LiveTV {
	if s.app == nil || s.app.livetv == nil {
		return nil
	}
	return s.app.livetv.Frontend()
}

func (s surfaceLiveTV) viewer(ctx context.Context, v *core.Viewer) (*core.User, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	viewer, err := s.app.users.ActiveUser(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	return s.app.safety.UserPolicy(viewer), nil
}
func (s surfaceLiveTV) liveChannel(viewer *core.Viewer, c livetv.Channel) frontend.LiveChannel {
	result := frontend.LiveChannel{ID: c.ID, Number: c.Number, Name: c.Name, Kind: c.Kind, Rating: c.Rating, Favorite: c.Favorite, Group: c.Group}
	if s.channelArtwork != nil {
		if image := s.channelArtwork(viewer, c); image.Path != "" {
			result.Images = []frontend.Image{image}
		}
	}
	return result
}
func (s surfaceLiveTV) Channels(ctx context.Context, v *core.Viewer) ([]frontend.LiveChannel, error) {
	ctx = core.WithLiveApp(ctx, s.surface)
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return nil, err
	}
	if s.app.livetv == nil {
		return nil, core.ErrNotAvailable
	}
	channels, err := s.app.livetv.AllowedChannels(ctx, viewer)
	if err == nil {
		s.app.livetv.NoteClientActivity()
	}
	if err != nil {
		return nil, err
	}
	out := make([]frontend.LiveChannel, 0, len(channels))
	for _, c := range channels {
		out = append(out, s.liveChannel(viewer, c))
	}
	return out, nil
}
func (s surfaceLiveTV) Guide(ctx context.Context, v *core.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
	page, err := s.guidePage(ctx, v, frontend.LiveGuidePageQuery{LiveGuideQuery: q}, false)
	return page.Items, err
}

func (s surfaceLiveTV) GuidePage(ctx context.Context, v *core.Viewer, pageQuery frontend.LiveGuidePageQuery) (frontend.LiveGuidePage, error) {
	return s.guidePage(ctx, v, pageQuery, true)
}

func (s surfaceLiveTV) guidePage(ctx context.Context, v *core.Viewer, pageQuery frontend.LiveGuidePageQuery, paged bool) (frontend.LiveGuidePage, error) {
	q := pageQuery.LiveGuideQuery
	ctx = core.WithLiveApp(ctx, s.surface)
	if !q.Until.After(q.From) || q.Until.Sub(q.From) > 7*24*time.Hour || q.Limit < 0 || q.Limit > maxLiveProgrammes || pageQuery.Offset < 0 {
		return frontend.LiveGuidePage{}, errLiveQuery
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.LiveGuidePage{}, err
	}
	if s.app.livetv == nil {
		return frontend.LiveGuidePage{}, core.ErrNotAvailable
	}
	s.app.livetv.NoteClientActivity()
	var permitted []livetv.Channel
	if len(q.Channels) > 0 {
		permitted, err = s.app.livetv.AllowedChannelsForIDs(ctx, viewer, q.Channels)
	} else {
		permitted, err = s.app.livetv.AllowedChannels(ctx, viewer)
	}
	if err != nil {
		return frontend.LiveGuidePage{}, err
	}
	selected := make(map[string]bool, len(q.Channels))
	for _, id := range q.Channels {
		selected[id] = true
	}
	allowed := make(map[string]bool, len(permitted))
	for _, c := range permitted {
		if len(selected) == 0 || selected[c.ID] {
			allowed[c.ID] = true
		}
	}
	// TODO: bound upstream guide materialization after policy filtering; retain
	// the existing Plex scheduling semantics while applying a response cap here.
	var programmes []livetv.Programme
	if len(q.Channels) > 0 {
		programmes, err = s.app.livetv.GuideForChannels(ctx, viewer, q.From, q.Until, q.Channels)
	} else {
		programmes, err = s.app.livetv.Guide(ctx, viewer, q.From, q.Until)
	}
	if err != nil {
		return frontend.LiveGuidePage{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = maxLiveProgrammes
	}
	// The source materializes the selected time window from its guide snapshot.
	// Fingerprint that authorized order without counting the media library or
	// hydrating titles outside this response page.
	fingerprint := sha256.New()
	encoder := json.NewEncoder(fingerprint)
	if paged {
		if err := encoder.Encode([]any{q.From.UTC(), q.Until.UTC(), q.Channels}); err != nil {
			return frontend.LiveGuidePage{}, err
		}
	}
	selectedPage := make([]livetv.Programme, 0, min(len(programmes), limit+1))
	index := 0
	for _, p := range programmes {
		if !allowed[p.ChannelID] {
			continue
		}
		if paged {
			if err := encoder.Encode([]any{p.ID, p.ChannelID, p.Start.UTC(), p.End.UTC(), p.Item.Key, p.Item.Title, p.Item.Summary, p.Item.Rating, p.Item.Thumb, p.Item.Art, p.Item.Genres}); err != nil {
				return frontend.LiveGuidePage{}, err
			}
		}
		if index >= pageQuery.Offset && len(selectedPage) <= limit {
			selectedPage = append(selectedPage, p)
		}
		index++
		if !paged && len(selectedPage) == limit {
			break
		}
	}
	snapshot := hex.EncodeToString(fingerprint.Sum(nil))
	if pageQuery.Snapshot != "" && pageQuery.Snapshot != snapshot {
		return frontend.LiveGuidePage{}, frontend.ErrLiveGuideStale
	}
	more := len(selectedPage) > limit
	selectedPage = selectedPage[:min(len(selectedPage), limit)]
	out := make([]frontend.LiveProgramme, 0, len(selectedPage))
	imageCache := make(map[core.RatingKey][]frontend.Image)
	for _, p := range selectedPage {
		item := frontend.LiveProgramme{ID: p.ID, ChannelID: p.ChannelID, Start: p.Start, End: p.End, Item: p.Item.Key, Title: p.Item.Title, Summary: p.Item.Summary, Rating: p.Item.Rating, ShowTitle: p.Item.ShowTitle, EpisodeTitle: p.Item.EpisodeTitle, Season: p.Item.Season, Episode: p.Item.Episode, Genres: p.Item.Genres}
		if s.programmeArtwork != nil {
			item.Images = s.programmeArtwork(viewer, p)
		}
		if p.Item.Key != 0 && s.app.catalog != nil {
			if title, e := s.app.catalog.Get(ctx, p.Item.Key); e == nil {
				if item.ShowTitle == "" {
					item.ShowTitle = title.ShowTitle
				}
				if item.EpisodeTitle == "" && title.Type == core.TypeEpisode {
					item.EpisodeTitle = title.Title
				}
				if item.Season == 0 {
					item.Season = title.Season
				}
				if item.Episode == 0 {
					item.Episode = title.Episode
				}
				if len(item.Genres) == 0 {
					item.Genres = title.Genres
				}
			}
			if s.artwork != nil && len(item.Images) == 0 {
				images, found := imageCache[p.Item.Key]
				if !found {
					var complete bool
					images, complete, _ = s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: q.Device, Item: p.Item.Key})
					if !complete {
						images = nil
					}
					imageCache[p.Item.Key] = images
				}
				item.Images = images
			}
		}
		out = append(out, item)
	}
	return frontend.LiveGuidePage{Items: out, Snapshot: snapshot, More: more}, nil
}
func normalizeLiveGUID(raw string, kind ids.Kind) (string, error) {
	n, err := ids.ParseGUID(kind, raw)
	if err != nil {
		return "", core.ErrNotFound
	}
	return ids.GUID(kind, n)
}
func (s surfaceLiveTV) ResolveChannelGUID(ctx context.Context, v *core.Viewer, guid string) (frontend.LiveChannel, error) {
	ctx = core.WithLiveApp(ctx, s.surface)
	normalized, err := normalizeLiveGUID(guid, ids.Channel)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	if s.app.livetv == nil {
		return frontend.LiveChannel{}, core.ErrNotAvailable
	}
	channels, err := s.app.livetv.Channels(ctx)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	var matched []livetv.Channel
	for _, c := range channels {
		if !c.Enabled {
			continue
		}
		id, _ := ids.StringGUID(ids.Channel, c.ID)
		if strings.EqualFold(id, normalized) {
			matched = append(matched, c)
		}
	}
	var cached func(context.Context, core.RatingKey) (*core.Title, error)
	if s.app.catalog != nil {
		cached = s.app.catalog.CachedGet
	}
	var found *frontend.LiveChannel
	for _, c := range matched {
		if err := s.app.livetv.AllowedChannelCached(ctx, viewer, c.ID, cached); err != nil {
			if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
				continue
			}
			return frontend.LiveChannel{}, err
		}
		if found != nil {
			return frontend.LiveChannel{}, core.ErrUnauthorized
		}
		row := s.liveChannel(viewer, c)
		found = &row
	}
	if found == nil {
		return frontend.LiveChannel{}, core.ErrNotFound
	}
	return *found, nil
}
func (s surfaceLiveTV) ResolveProgrammeGUID(ctx context.Context, v *core.Viewer, guid string, q frontend.LiveGuideQuery) (frontend.LiveProgramme, error) {
	normalized, err := normalizeLiveGUID(guid, ids.Programme)
	if err != nil {
		return frontend.LiveProgramme{}, err
	}
	rows, err := s.Guide(ctx, v, q)
	if err != nil {
		return frontend.LiveProgramme{}, err
	}
	var found *frontend.LiveProgramme
	for _, row := range rows {
		id, _ := ids.StringGUID(ids.Programme, row.ID)
		if strings.EqualFold(id, normalized) {
			if found != nil {
				return frontend.LiveProgramme{}, core.ErrUnauthorized
			}
			copy := row
			found = &copy
		}
	}
	if found == nil {
		return frontend.LiveProgramme{}, core.ErrNotFound
	}
	return *found, nil
}

func (s surfaceLiveTV) VisibleTo(ctx context.Context, u *frontend.User) bool {
	h := s.host()
	return h != nil && h.VisibleTo(ctx, u)
}
func (s surfaceLiveTV) ServeXMLTV(w http.ResponseWriter, r *http.Request, u *frontend.User) {
	if h := s.host(); h != nil {
		h.ServeXMLTV(w, r, u)
	}
}
func (s surfaceLiveTV) Stop(ctx context.Context, id string, u *frontend.User) error {
	h := s.host()
	if h == nil {
		return core.ErrNotAvailable
	}
	return h.Stop(ctx, id, u)
}
func (s surfaceLiveTV) ServeDASHPart(w http.ResponseWriter, r *http.Request, id string, u *frontend.User, rep, n int, init bool) {
	if h := s.host(); h != nil {
		h.ServeDASHPart(w, r, id, u, rep, n, init)
	}
}
func (s surfaceLiveTV) ServePlaylist(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) {
	if h := s.host(); h != nil {
		h.ServePlaylist(w, r, id, u)
	}
}
func (s surfaceLiveTV) ServeRelaySegment(w http.ResponseWriter, r *http.Request, id string, n int, u *frontend.User) {
	if h := s.host(); h != nil {
		h.ServeRelaySegment(w, r, id, n, u)
	}
}
func (s surfaceLiveTV) ServeResource(w http.ResponseWriter, r *http.Request, id, resource string, u *frontend.User) {
	if h := s.host(); h != nil {
		h.ServeResource(w, r, id, resource, u)
	}
}
func (s surfaceLiveTV) AllowedChannels(ctx context.Context, u *frontend.User) ([]frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return nil, core.ErrNotAvailable
	}
	return h.AllowedChannels(ctx, u)
}
func (s surfaceLiveTV) AllowedChannel(ctx context.Context, u *frontend.User, id string) (frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVChannel{}, core.ErrNotAvailable
	}
	return h.AllowedChannel(ctx, u, id)
}
func (s surfaceLiveTV) Channel(ctx context.Context, id string) (frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVChannel{}, core.ErrNotAvailable
	}
	return h.Channel(ctx, id)
}
func (s surfaceLiveTV) ChannelForViewer(ctx context.Context, u *frontend.User, id string) (frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVChannel{}, core.ErrNotAvailable
	}
	return h.ChannelForViewer(ctx, u, id)
}
func (s surfaceLiveTV) AllChannels(ctx context.Context) ([]frontend.LiveTVChannel, error) {
	h := s.host()
	if h == nil {
		return nil, core.ErrNotAvailable
	}
	return h.AllChannels(ctx)
}
func (s surfaceLiveTV) ChannelPermitted(ctx context.Context, u *frontend.User, id string) (bool, error) {
	h := s.host()
	if h == nil {
		return false, core.ErrNotAvailable
	}
	return h.ChannelPermitted(ctx, u, id)
}
func (s surfaceLiveTV) LineupChangedFor(u *frontend.User) time.Time {
	if h := s.host(); h != nil {
		return h.LineupChangedFor(u)
	}
	return time.Time{}
}
func (s surfaceLiveTV) GuideForChannels(ctx context.Context, u *frontend.User, from, until time.Time, channelIDs []string) ([]frontend.LiveTVProgramme, error) {
	h := s.host()
	if h == nil {
		return nil, core.ErrNotAvailable
	}
	return h.GuideForChannels(ctx, u, from, until, channelIDs)
}
func (s surfaceLiveTV) GuideWindow(ctx context.Context, u *frontend.User, from, until time.Time) ([]frontend.LiveTVProgramme, error) {
	h := s.host()
	if h == nil {
		return nil, core.ErrNotAvailable
	}
	return h.GuideWindow(ctx, u, from, until)
}
func (s surfaceLiveTV) GuideChannelLogo(ctx context.Context, c frontend.LiveTVChannel) string {
	if h := s.host(); h != nil {
		return h.GuideChannelLogo(ctx, c)
	}
	return ""
}
func (s surfaceLiveTV) TuneWithSession(ctx context.Context, u *frontend.User, id string, caps frontend.ClientCaps, sessionID string) (frontend.LiveTVSessionInfo, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVSessionInfo{}, core.ErrNotAvailable
	}
	return h.TuneWithSession(ctx, u, id, liveTVClientCaps(caps), sessionID)
}
func (s surfaceLiveTV) Sessions(ctx context.Context, u *frontend.User) []frontend.LiveTVSessionInfo {
	if h := s.host(); h != nil {
		return h.Sessions(ctx, u)
	}
	return nil
}
func (s surfaceLiveTV) Session(ctx context.Context, id string, u *frontend.User) (frontend.LiveTVSessionInfo, error) {
	h := s.host()
	if h == nil {
		return frontend.LiveTVSessionInfo{}, core.ErrNotAvailable
	}
	return h.Session(ctx, id, u)
}
func (s surfaceLiveTV) ServeDASH(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) error {
	h := s.host()
	if h == nil {
		return core.ErrNotAvailable
	}
	return h.ServeDASH(w, r, id, u)
}
func (s surfaceLiveTV) ServeHTTP(w http.ResponseWriter, r *http.Request, id string, u *frontend.User) error {
	h := s.host()
	if h == nil {
		return core.ErrNotAvailable
	}
	return h.ServeHTTP(w, r, id, u)
}
func (s surfaceLiveTV) ViewingSessions() []*frontend.PlaySession {
	if h := s.host(); h != nil {
		return h.ViewingSessions()
	}
	return nil
}
func (s surfaceLiveTV) NoteClientActivity() {
	if h := s.host(); h != nil {
		h.NoteClientActivity()
	}
}
func (s surfaceLiveTV) AuthenticateMediaGrant(r *http.Request, active func(context.Context, int64) *frontend.User) (*frontend.User, bool) {
	if h := s.host(); h != nil {
		return h.AuthenticateMediaGrant(r, active)
	}
	return nil, false
}
func (s surfaceLiveTV) LogoKey() []byte {
	if s.app != nil && s.app.cfg != nil {
		return s.app.cfg.DerivedKey("ether-live-logo-v1")
	}
	return nil
}

var _ frontend.LiveTV = surfaceLiveTV{}
