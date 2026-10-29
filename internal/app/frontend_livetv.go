package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/livetv"
)

const maxLiveChannels = 4096
const maxLiveProgrammes = 10000

var errLiveQuery = errors.New("invalid live guide query")

type surfaceLiveTV struct {
	app            *App
	channelArtwork func(*core.Viewer, livetv.Channel) frontend.Image
	artwork        frontend.Artwork
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
	result := frontend.LiveChannel{ID: c.ID, Number: c.Number, Name: c.Name, Kind: c.Kind, Rating: c.Rating, Favorite: c.Favorite}
	if s.channelArtwork != nil {
		if image := s.channelArtwork(viewer, c); image.Path != "" {
			result.Images = []frontend.Image{image}
		}
	}
	return result
}
func (s surfaceLiveTV) Channels(ctx context.Context, v *core.Viewer) ([]frontend.LiveChannel, error) {
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return nil, err
	}
	if s.app.livetv == nil {
		return nil, core.ErrNotAvailable
	}
	channels, err := s.app.livetv.AllowedChannels(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if len(channels) > maxLiveChannels {
		return nil, errLiveQuery
	}
	out := make([]frontend.LiveChannel, 0, len(channels))
	for _, c := range channels {
		out = append(out, s.liveChannel(viewer, c))
	}
	return out, nil
}
func (s surfaceLiveTV) Guide(ctx context.Context, v *core.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
	if !q.Until.After(q.From) || q.Until.Sub(q.From) > 7*24*time.Hour || len(q.Channels) > maxLiveChannels || q.Limit < 0 || q.Limit > maxLiveProgrammes {
		return nil, errLiveQuery
	}
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return nil, err
	}
	if s.app.livetv == nil {
		return nil, core.ErrNotAvailable
	}
	permitted, err := s.app.livetv.AllowedChannels(ctx, viewer)
	if err != nil {
		return nil, err
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
	programmes, err := s.app.livetv.Guide(ctx, viewer, q.From, q.Until)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = maxLiveProgrammes
	}
	out := make([]frontend.LiveProgramme, 0, min(len(programmes), limit))
	imageCache := make(map[core.RatingKey][]frontend.Image)
	for _, p := range programmes {
		if !allowed[p.ChannelID] {
			continue
		}
		item := frontend.LiveProgramme{ID: p.ID, ChannelID: p.ChannelID, Start: p.Start, End: p.End, Item: p.Item.Key, Title: p.Item.Title, Summary: p.Item.Summary, Rating: p.Item.Rating}
		if p.Item.Key != 0 && s.app.catalog != nil {
			if title, e := s.app.catalog.Get(ctx, p.Item.Key); e == nil {
				item.ShowTitle = title.ShowTitle
				item.EpisodeTitle = title.Title
				item.Season = title.Season
				item.Episode = title.Episode
			}
			if s.artwork != nil {
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
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
func normalizeLiveGUID(raw string, kind ids.Kind) (string, error) {
	n, err := ids.ParseGUID(kind, raw)
	if err != nil {
		return "", core.ErrNotFound
	}
	return ids.GUID(kind, n)
}
func (s surfaceLiveTV) ResolveChannelGUID(ctx context.Context, v *core.Viewer, guid string) (frontend.LiveChannel, error) {
	normalized, err := normalizeLiveGUID(guid, ids.Channel)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	rows, err := s.Channels(ctx, v)
	if err != nil {
		return frontend.LiveChannel{}, err
	}
	var found *frontend.LiveChannel
	for _, row := range rows {
		id, _ := ids.StringGUID(ids.Channel, row.ID)
		if strings.EqualFold(id, normalized) {
			if found != nil {
				return frontend.LiveChannel{}, core.ErrUnauthorized
			}
			copy := row
			found = &copy
		}
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

var _ frontend.LiveTV = surfaceLiveTV{}
