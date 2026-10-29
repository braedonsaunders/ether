package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) mutate(w http.ResponseWriter, r *http.Request, kind string) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("itemId"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch kind {
	case "played":
		if r.Method == "POST" {
			err = i.host.Experience.SetPlayed(r.Context(), v, key)
		} else {
			err = i.host.Experience.SetUnplayed(r.Context(), v, key)
		}
	case "favorite":
		err = i.host.Experience.SetFavorite(r.Context(), v, key, r.Method == "POST")
	}
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	data, err := i.host.Experience.UserData(r.Context(), v, []frontend.RatingKey{key})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	st := data[key]
	id := ids.RatingKeyGUID(key)
	jsonReply(w, map[string]any{"Key": id, "ItemId": id, "Played": st.ViewCount > 0, "PlayCount": st.ViewCount, "PlaybackPositionTicks": st.ViewOffset.Nanoseconds() / 100, "Rating": st.UserRating})
}
func (i *instance) mutationRoutes() error {
	for _, path := range []struct{ pattern, kind string }{{"/UserPlayedItems/{itemId}", "played"}, {"/Users/{userId}/PlayedItems/{itemId}", "played"}, {"/UserFavoriteItems/{itemId}", "favorite"}, {"/Users/{userId}/FavoriteItems/{itemId}", "favorite"}} {
		for _, method := range []string{"POST", "DELETE"} {
			kind := path.kind
			if err := i.router.Handle(method, path.pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.mutate(w, r, kind) })); err != nil {
				return err
			}
		}
	}
	return nil
}
