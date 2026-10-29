package experience

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
)

// PlaylistRowSource is scoped to a viewer's own playlists.
type PlaylistRowSource interface {
	List(context.Context, int64) ([]core.Playlist, error)
}

// PlaylistEntries returns newest playlists first. Cross-viewer records are
// discarded even if a source accidentally returns them.
func PlaylistEntries(ctx context.Context, viewer *core.User, source PlaylistRowSource, limit int) ([]frontend.RowEntry, bool, error) {
	if viewer == nil || source == nil {
		return nil, false, errors.New("experience: missing viewer or playlist source")
	}
	playlists, err := source.List(ctx, viewer.ID)
	if err != nil {
		return nil, false, err
	}
	owned := make([]core.Playlist, 0, len(playlists))
	for _, p := range playlists {
		if p.UserID == viewer.ID {
			p.Items = nil // Item access is checked separately when the playlist is opened.
			owned = append(owned, p)
		}
	}
	sort.SliceStable(owned, func(i, j int) bool { return owned[i].UpdatedAt.After(owned[j].UpdatedAt) })
	more := limit > 0 && len(owned) > limit
	if more {
		owned = owned[:limit]
	}
	out := make([]frontend.RowEntry, 0, len(owned))
	for i := range owned {
		out = append(out, frontend.RowEntry{Kind: frontend.RowEntryPlaylist, Playlist: &owned[i]})
	}
	return out, more, nil
}

// LiveGuideSource must enforce channel and programme access for the viewer.
type LiveGuideSource interface {
	AllowedChannels(context.Context, *core.User) ([]livetv.Channel, error)
	Guide(context.Context, *core.User, time.Time, time.Time) ([]livetv.Programme, error)
}

// LiveEntries projects allowed channels and their current and next programme.
// The caller supplies the guide horizon; stream and provider URLs never leave
// this projection. Image is an optional server-controlled logo resolver.
func LiveEntries(ctx context.Context, viewer *core.User, source LiveGuideSource, now, until time.Time, image func(livetv.Channel) string) ([]frontend.RowEntry, error) {
	if viewer == nil || source == nil || !until.After(now) {
		return nil, errors.New("experience: invalid live guide request")
	}
	channels, err := source.AllowedChannels(ctx, viewer)
	if err != nil {
		return nil, err
	}
	programmes, err := source.Guide(ctx, viewer, now, until)
	if err != nil {
		return nil, err
	}
	byChannel := make(map[string][]livetv.Programme)
	for _, p := range programmes {
		if p.End.After(now) && p.Start.Before(until) {
			byChannel[p.ChannelID] = append(byChannel[p.ChannelID], p)
		}
	}
	out := make([]frontend.RowEntry, 0, len(channels))
	for _, c := range channels {
		channel := &frontend.RowLiveChannel{ID: c.ID, Number: c.Number, Name: c.Name, GuideReference: c.GuideID}
		if image != nil {
			channel.Logo = image(c)
		}
		ps := byChannel[c.ID]
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].Start.Before(ps[j].Start) })
		for _, p := range ps {
			item := &frontend.RowProgramme{ID: p.ID, Title: p.Item.Title, Start: p.Start, End: p.End, Item: p.Item.Key}
			if !p.Start.After(now) && p.End.After(now) && channel.Current == nil {
				channel.Current = item
			} else if p.Start.After(now) && channel.Next == nil {
				channel.Next = item
			}
		}
		out = append(out, frontend.RowEntry{Kind: frontend.RowEntryLiveChannel, LiveChannel: channel})
	}
	return out, nil
}
