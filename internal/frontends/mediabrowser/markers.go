package mediabrowser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func (i *instance) mediaSegments(w http.ResponseWriter, r *http.Request) {
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
		scope, scopeErr := i.versionScope(r, v)
		if scopeErr != nil || i.host.VersionHandles == nil {
			http.NotFound(w, r)
			return
		}
		target, resolveErr := i.host.VersionHandles.Resolve(i.identityContext(r), scope, r.PathValue("itemId"))
		if resolveErr != nil || target.Item == 0 {
			http.NotFound(w, r)
			return
		}
		key = target.Item
	}
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	filter := map[string]bool{}
	for _, value := range route.Query(r.URL.Query(), "includeSegmentTypes") {
		for _, t := range strings.Split(value, ",") {
			filter[strings.ToLower(t)] = true
		}
	}
	items := []any{}
	for _, marker := range detail.Markers {
		kind := ""
		switch strings.ToLower(marker.Type) {
		case "intro":
			kind = "Intro"
		case "credits", "outro":
			kind = "Outro"
		case "recap":
			kind = "Recap"
		case "commercial":
			kind = "Commercial"
		case "preview":
			kind = "Preview"
		}
		if kind == "" || marker.Start < 0 || marker.End <= marker.Start || len(filter) > 0 && !filter[strings.ToLower(kind)] {
			continue
		}
		item := ids.RatingKeyGUID(key)
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%d", item, kind, marker.Start, marker.End)))
		items = append(items, map[string]any{"Id": hex.EncodeToString(sum[:16]), "ItemId": item, "Type": kind, "StartTicks": marker.Start.Nanoseconds() / 100, "EndTicks": marker.End.Nanoseconds() / 100})
	}
	jsonReply(w, map[string]any{"Items": items, "TotalRecordCount": len(items), "StartIndex": 0})
}
