package silo

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/langtag"
)

func (i *instance) subtitleRoutes() error {
	if err := i.router.Handle("POST", "/api/v2/subtitles/search", http.HandlerFunc(i.searchSubtitles)); err != nil {
		return err
	}
	return i.router.Handle("POST", "/api/v2/subtitles/download", http.HandlerFunc(i.downloadSubtitle))
}

type subtitleSearchBody struct {
	FileID    string   `json:"media_file_id"`
	Languages []string `json:"languages"`
}

type subtitleDownloadBody struct {
	FileID          string  `json:"media_file_id"`
	Provider        string  `json:"provider"`
	SubtitleID      string  `json:"subtitle_id"`
	Language        string  `json:"language"`
	ReleaseName     string  `json:"release_name"`
	Score           float64 `json:"score"`
	HearingImpaired bool    `json:"hearing_impaired"`
}

func (i *instance) searchSubtitles(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var body subtitleSearchBody
	if decode(r, &body) != nil || body.FileID == "" || body.Languages == nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	key, ok := i.authorizedFile(w, r, viewer, body.FileID)
	if !ok {
		return
	}
	warnings := []string{}
	results := []any{}
	if i.host.Subtitles == nil || !i.host.Subtitles.Enabled() {
		warnings = append(warnings, "Subtitle search is not configured")
		reply(w, http.StatusOK, map[string]any{"results": results, "warnings": warnings})
		return
	}
	languages := body.Languages
	if len(languages) > 8 {
		languages = languages[:8]
		warnings = append(warnings, "Only the first 8 languages were searched")
	}
	seen := map[string]bool{}
	for _, requested := range languages {
		language := langtag.Normalize(requested)
		if !subtitleLanguage.MatchString(language) {
			warnings = append(warnings, "Skipped an unrecognized language")
			continue
		}
		offers, err := i.host.Subtitles.Search(r.Context(), viewer, device(r), key, language)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) || errors.Is(err, frontend.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			warnings = append(warnings, "Subtitle search is unavailable")
			continue
		}
		for _, offer := range offers {
			language := langtag.Normalize(offer.Language)
			if !subtitleLanguage.MatchString(language) || offer.ID == "" || seen[offer.ID] {
				continue
			}
			seen[offer.ID] = true
			name := offer.Name
			if name == "" {
				name = "Subtitle"
			}
			format := offer.Format
			if format == "" {
				format = "srt"
			}
			results = append(results, map[string]any{
				"id": offer.ID, "provider": "opensubtitles", "language": language, "release_name": name,
				"format": format, "score": offer.Rating, "downloads": offer.Downloads, "hearing_impaired": offer.HearingImpaired,
			})
		}
	}
	reply(w, http.StatusOK, map[string]any{"results": results, "warnings": warnings})
}

func (i *instance) downloadSubtitle(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var body subtitleDownloadBody
	if decode(r, &body) != nil || body.FileID == "" || body.SubtitleID == "" || body.Provider == "" || body.Language == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(body.Provider, "opensubtitles") || !subtitleLanguage.MatchString(langtag.Normalize(body.Language)) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	key, ok := i.authorizedFile(w, r, viewer, body.FileID)
	if !ok {
		return
	}
	if i.host.Subtitles == nil || !i.host.Subtitles.Enabled() {
		http.NotFound(w, r)
		return
	}
	if _, err := i.host.Subtitles.Download(r.Context(), viewer, device(r), key, body.SubtitleID); err != nil {
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	reply(w, http.StatusOK, map[string]any{"subtitle": map[string]any{
		"id": body.SubtitleID, "media_file_id": body.FileID, "provider": "opensubtitles",
		"language": langtag.Normalize(body.Language), "format": "srt", "release_name": body.ReleaseName,
		"score": body.Score, "hearing_impaired": body.HearingImpaired,
		"created_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"timing":     map[string]any{"offset_ms": 0, "scale": 1.0},
	}})
}

func (i *instance) authorizedFile(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, fileID string) (core.RatingKey, bool) {
	if i.host.SourceOffers == nil || i.host.Experience == nil {
		http.NotFound(w, r)
		return 0, false
	}
	var client core.ClientCaps
	if i.host.Capabilities != nil {
		built, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: "silo", ClientID: device(r).ID, Product: device(r).Client, Version: device(r).Version, DeviceName: device(r).DeviceName}, UserAgent: r.UserAgent()})
		if err != nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return 0, false
		}
		client = built.Client
	}
	offer, err := i.host.SourceOffers.Resolve(r.Context(), frontend.VersionScope{Viewer: viewer, Device: device(r), Protocol: "silo", Caps: client}, fileID)
	if err != nil || offer.Target.Item == 0 {
		http.NotFound(w, r)
		return 0, false
	}
	if _, err = i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: offer.Target.Item}); err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return offer.Target.Item, true
}

var subtitleLanguage = regexp.MustCompile(`^(?:[a-z]{2,3}(?:-[0-9A-Za-z]{1,8})*|x(?:-[0-9A-Za-z]{1,8})+)$`)
