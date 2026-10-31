package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// Extra playback is not yet available through the neutral broker. Only
// return an empty result after checking access to the requested parent.
func (i *instance) unsupportedExtras(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		scope, scopeErr := i.versionScope(r, viewer)
		if scopeErr != nil || i.host.VersionHandles == nil {
			http.NotFound(w, r)
			return
		}
		target, resolveErr := i.host.VersionHandles.Resolve(i.identityContext(r), scope, r.PathValue("id"))
		if resolveErr != nil || target.Item == 0 {
			http.NotFound(w, r)
			return
		}
		key = target.Item
	}
	if i.host.Experience == nil {
		http.NotFound(w, r)
		return
	}
	if _, err = i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key}); err != nil {
		http.NotFound(w, r)
		return
	}
	jsonReply(w, []any{})
}
