package plex

import (
	"context"
	"errors"
	"reflect"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

type plexHomeSource struct{ rows []frontend.Row }

func (s plexHomeSource) Rows(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
	return s.rows, nil
}

// arrangePlexHome keeps wire sidecars local; experience sees only entries
// whose identity occurs in the already rendered and authorized response.
func (s *Server) arrangePlexHome(ctx context.Context, pr *Request, hc core.HubContext, hubs []model.Hub, projections map[string]frontend.Row) []model.Hub {
	if pr == nil || pr.User == nil {
		return s.finalizeHomeRows(ctx, pr, hc, "home", hubs)
	}
	rows := neutralPlexHomeRows(hubs, projections)
	// Strict policy applies before neutral data can leave experience. The
	// Plex sidecars remain untouched for unrestricted-viewer parity.
	for i := range rows {
		filtered := rows[i].Entries[:0:0]
		for _, entry := range rows[i].Entries {
			if entry.Title != nil && (!s.titleVisible(pr.User, entry.Title) || s.viewerRestricted(pr.User) && !s.visibleTitle(ctx, pr.User, entry.Title)) {
				continue
			}
			if entry.Playlist != nil && entry.Playlist.UserID != pr.User.ID {
				continue
			}
			filtered = append(filtered, entry)
		}
		rows[i].Entries = filtered
	}
	var fallback []model.Hub
	layoutDone := false
	legacy := func() []model.Hub {
		if layoutDone {
			return fallback
		}
		return s.finalizeHomeRows(ctx, pr, hc, "home", hubs)
	}
	allowedEntries := map[*core.Title]bool{}
	allowedPlaylists := map[*core.Playlist]bool{}
	allowedChannels := map[*frontend.RowLiveChannel]bool{}
	for _, row := range rows {
		for _, entry := range row.Entries {
			if entry.Title != nil && s.titleVisible(pr.User, entry.Title) {
				allowedEntries[entry.Title] = true
			}
			if entry.Playlist != nil && pr.User != nil && entry.Playlist.UserID == pr.User.ID {
				allowedPlaylists[entry.Playlist] = true
			}
			if entry.LiveChannel != nil {
				allowedChannels[entry.LiveChannel] = true
			}
		}
	}
	svc, err := experience.NewPlexRowsService(experience.PlexRowDeps{
		Source: plexHomeSource{rows},
		Active: func(_ context.Context, u *core.User) bool { return u != nil && pr.User != nil && u.ID == pr.User.ID },
		AllowRow: func(_ context.Context, _ *core.User, row frontend.Row) bool {
			i, e := strconv.Atoi(row.ID)
			return e == nil && i >= 0 && i < len(rows)
		},
		AllowEntry: func(_ context.Context, _ *core.User, e frontend.RowEntry) bool {
			return allowedEntries[e.Title] || allowedPlaylists[e.Playlist] || allowedChannels[e.LiveChannel]
		},
		Layout: func(ctx context.Context, _ frontend.RowRequest, _ []frontend.Row) ([]frontend.Row, error) {
			ordered := s.finalizeHomeRows(ctx, pr, hc, "home", hubs)
			fallback, layoutDone = ordered, true
			result := make([]frontend.Row, 0, len(ordered))
			for _, hub := range ordered {
				matched := false
				for i := range hubs {
					if reflect.DeepEqual(hubs[i], hub) {
						result = append(result, rows[i])
						matched = true
						break
					}
				}
				if !matched {
					return nil, errors.New("Plex layout returned an unknown sidecar")
				}
			}
			return result, nil
		}})
	if err != nil {
		return legacy()
	}
	ordered, err := svc.Rows(ctx, frontend.RowRequest{Viewer: pr.User}, core.Protocol("plex"))
	if err != nil {
		return legacy()
	}
	out := make([]model.Hub, 0, len(ordered))
	for _, row := range ordered {
		i, e := strconv.Atoi(row.ID)
		if e != nil || i < 0 || i >= len(hubs) {
			return legacy()
		}
		out = append(out, hubs[i])
	}
	return out
}

func neutralPlexHomeRows(hubs []model.Hub, projections map[string]frontend.Row) []frontend.Row {
	rows := make([]frontend.Row, len(hubs))
	for i, hub := range hubs {
		row := projections[hub.HubIdentifier]
		row.ID = strconv.Itoa(i)
		allowed := make(map[string]bool, len(hub.Metadata))
		channels := make(map[string]bool)
		for _, metadata := range hub.Metadata {
			allowed[metadata.RatingKey] = true
			for _, media := range metadata.Media {
				for _, attr := range media.Extra {
					if attr.Name == "channelIdentifier" {
						if id, ok := attr.Value.(string); ok {
							channels[id] = true
						}
					}
				}
			}
		}
		entries := row.Entries[:0:0]
		for _, entry := range row.Entries {
			if entry.Title != nil && allowed[entry.Title.Key.String()] || entry.Playlist != nil && allowed[entry.Playlist.Key.String()] || entry.LiveChannel != nil && (allowed["channel-"+entry.LiveChannel.ID] || channels[entry.LiveChannel.ID]) {
				entries = append(entries, entry)
			}
		}
		row.Entries = entries
		rows[i] = row
	}
	return rows
}
