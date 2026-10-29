package mediabrowser

import (
	"context"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// eventUserData resolves non-title subjects through Experience before
// constructing a protocol ID. A failed policy check never produces an event.
func (i *instance) eventUserData(ctx context.Context, viewer *frontend.Viewer, dev frontend.Device, event frontend.Event) (string, frontend.WatchStatus, bool) {
	if event.SubjectKind != "" && event.SubjectID != "" {
		resolved, err := i.host.Experience.ResolveEventSubject(ctx, viewer, dev, frontend.FavoriteRef{Kind: frontend.FavoriteKind(event.SubjectKind), ID: event.SubjectID})
		if err != nil {
			return "", frontend.WatchStatus{}, false
		}
		id := ""
		switch resolved.Ref.Kind {
		case frontend.FavoritePerson:
			id = resolved.PersonFacetID
		case frontend.FavoriteChannel:
			id, err = ids.StringGUID(ids.Channel, resolved.Ref.ID)
		case frontend.FavoriteTitle, frontend.FavoriteCollection:
			var n int64
			n, err = ids.ParseDecimal(resolved.Ref.ID)
			if err == nil {
				id = ids.RatingKeyGUID(frontend.RatingKey(n))
			}
		default:
			return "", frontend.WatchStatus{}, false
		}
		if err != nil || id == "" {
			return "", frontend.WatchStatus{}, false
		}
		status := frontend.WatchStatus{IsFavorite: resolved.Favorite}
		if resolved.UserData != nil {
			status = *resolved.UserData
		}
		return id, status, true
	}
	if event.Item == 0 {
		return "", frontend.WatchStatus{}, false
	}
	states, err := i.host.Experience.UserData(ctx, viewer, []frontend.RatingKey{event.Item})
	if err != nil {
		return "", frontend.WatchStatus{}, false
	}
	status, ok := states[event.Item]
	return ids.RatingKeyGUID(event.Item), status, ok
}
