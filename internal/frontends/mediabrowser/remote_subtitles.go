package mediabrowser

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/langtag"
)

func (i *instance) remoteSubtitles(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.protocol == "emby" {
		if values := route.Query(r.URL.Query(), "MediaSourceId"); len(values) != 1 || values[0] == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}
	language := langtag.Normalize(r.PathValue("language"))
	if language == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	key, ok := i.subtitleItem(w, r, viewer)
	if !ok {
		return
	}
	if i.host.Subtitles == nil || !i.host.Subtitles.Enabled() {
		jsonReply(w, []any{})
		return
	}
	offers, err := i.host.Subtitles.Search(r.Context(), viewer, device(r), key, language)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	perfect := queryOptionalBool(r, "IsPerfectMatch")
	forced := queryOptionalBool(r, "IsForced")
	impaired := queryOptionalBool(r, "IsHearingImpaired")
	items := []any{}
	for _, offer := range offers {
		if offer.ID == "" {
			continue
		}
		if perfect != nil && *perfect != offer.HashMatch {
			continue
		}
		if forced != nil && *forced != offer.Forced {
			continue
		}
		if impaired != nil && *impaired != offer.HearingImpaired {
			continue
		}
		items = append(items, i.remoteSubtitleDTO(r.PathValue("language"), language, offer))
	}
	jsonReply(w, items)
}

func (i *instance) downloadSubtitle(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.protocol == "emby" {
		if values := route.Query(r.URL.Query(), "MediaSourceId"); len(values) != 1 || values[0] == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}
	offerID := strings.TrimSpace(r.PathValue("subtitleId"))
	if offerID == "" || strings.ContainsAny(offerID, "/\\") {
		http.NotFound(w, r)
		return
	}
	key, ok := i.subtitleItem(w, r, viewer)
	if !ok {
		return
	}
	if i.host.Subtitles == nil || !i.host.Subtitles.Enabled() {
		http.NotFound(w, r)
		return
	}
	index, err := i.host.Subtitles.Download(r.Context(), viewer, device(r), key, offerID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if i.protocol == "jellyfin" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	jsonReply(w, map[string]any{"NewIndex": index})
}

func (i *instance) subtitleItem(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer) (core.RatingKey, bool) {
	if i.host.Experience == nil {
		http.NotFound(w, r)
		return 0, false
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	if _, err = i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key}); err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return key, true
}

func (i *instance) remoteSubtitleDTO(requested, normalized string, offer frontend.SubtitleOffer) map[string]any {
	name := offer.Name
	if name == "" {
		name = "Subtitle"
	}
	iso3 := threeLetterLanguage(requested, normalized)
	if i.protocol == "jellyfin" {
		item := map[string]any{
			"Id": offer.ID, "ProviderName": offer.Provider, "Name": name, "Format": offer.Format,
			"ThreeLetterISOLanguageName": iso3, "IsHashMatch": offer.HashMatch,
			"Forced": offer.Forced, "HearingImpaired": offer.HearingImpaired,
			"AiTranslated": offer.AITranslated, "MachineTranslated": false,
		}
		if offer.Author != "" {
			item["Author"] = offer.Author
		}
		if offer.Downloads > 0 {
			item["DownloadCount"] = offer.Downloads
		}
		if offer.Rating > 0 {
			item["CommunityRating"] = offer.Rating
		}
		return item
	}
	item := map[string]any{
		"Id": offer.ID, "ProviderName": offer.Provider, "Name": name, "Format": offer.Format,
		"Language": normalized, "ThreeLetterISOLanguageName": iso3,
		"IsHashMatch": offer.HashMatch, "IsForced": offer.Forced, "IsHearingImpaired": offer.HearingImpaired,
	}
	if offer.Author != "" {
		item["Author"] = offer.Author
	}
	if offer.Downloads > 0 {
		item["DownloadCount"] = offer.Downloads
	}
	if offer.Rating > 0 {
		item["CommunityRating"] = offer.Rating
	}
	return item
}

func queryOptionalBool(r *http.Request, name string) *bool {
	values := route.Query(r.URL.Query(), name)
	if len(values) != 1 {
		return nil
	}
	value, err := strconv.ParseBool(values[0])
	if err != nil {
		return nil
	}
	return &value
}

func threeLetterLanguage(requested, normalized string) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if len(requested) == 3 {
		return requested
	}
	if code, ok := iso6392[normalized]; ok {
		return code
	}
	if len(normalized) == 3 {
		return normalized
	}
	return normalized
}

var iso6392 = map[string]string{
	"en": "eng", "fr": "fra", "de": "deu", "es": "spa", "it": "ita", "pt": "por",
	"nl": "nld", "sv": "swe", "no": "nor", "da": "dan", "fi": "fin", "pl": "pol",
	"ru": "rus", "ja": "jpn", "ko": "kor", "zh": "zho", "ar": "ara", "hi": "hin",
	"tr": "tur", "cs": "ces", "el": "ell", "he": "heb", "hu": "hun", "ro": "ron",
	"uk": "ukr", "th": "tha", "vi": "vie", "id": "ind",
}
