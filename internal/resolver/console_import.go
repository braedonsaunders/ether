package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// Quality profile import sources.
const (
	ImportSourceTrash       = "trash"
	ImportSourceDictionarry = "dictionarry"
	ImportSourceProfilarr   = "profilarr"
)

// importRequest names a TRaSH or Profilarr (Dictionarry) profile (app +
// optional source + profile id) or carries pasted profile JSON; name is the
// owner's choice for the saved profile.
type importRequest struct {
	App     string `json:"app"`
	Source  string `json:"source"`
	Profile string `json:"profile"`
	JSON    string `json:"json"`
	Name    string `json:"name"`
}

// maxImportBody bounds a pasted profile; TRaSH's largest is ~10 KiB.
const maxImportBody = 1 << 20

func (h *consoleAPI) trashProfiles(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if !validApp(app) {
		consoleError(w, http.StatusBadRequest, "invalid", "app must be radarr or sonarr")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	items, err := h.deps.Guides.Profiles(ctx, app)
	if err != nil {
		h.importFail(w, err)
		return
	}
	if items == nil {
		items = []TrashProfile{}
	}
	consoleJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *consoleAPI) dictionarryProfiles(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	if !validApp(app) {
		consoleError(w, http.StatusBadRequest, "invalid", "app must be radarr or sonarr")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	items, err := h.deps.Dictionarry.Profiles(ctx, app)
	if err != nil {
		h.dictionarryFail(w, err)
		return
	}
	if items == nil {
		items = []TrashProfile{}
	}
	consoleJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *consoleAPI) previewImport(w http.ResponseWriter, r *http.Request) {
	var in importRequest
	if !decodeImport(w, r, &in) {
		return
	}
	plan, ok := h.plan(w, r, in)
	if !ok {
		return
	}
	consoleJSON(w, http.StatusOK, plan)
}

func (h *consoleAPI) applyImport(w http.ResponseWriter, r *http.Request) {
	var in importRequest
	if !decodeImport(w, r, &in) {
		return
	}
	if h.deps.SaveConfig == nil {
		consoleError(w, http.StatusNotImplemented, "not_implemented", "profile imports are not available")
		return
	}
	plan, ok := h.plan(w, r, in)
	if !ok {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = plan.Profile.Name
	}
	if err := h.deps.SaveConfig(func(c *config.Config) error { return plan.Apply(c, name) }); err != nil {
		switch {
		case errors.Is(err, ErrProfileExists):
			consoleError(w, http.StatusConflict, "exists", fmt.Sprintf("A profile named %q already exists; choose another name.", name))
		case errors.Is(err, ErrImportRejected):
			consoleError(w, http.StatusBadRequest, "validation", err.Error())
		default:
			h.deps.Logger.Error("quality profile import: save failed", "err", err)
			consoleError(w, http.StatusInternalServerError, "save_failed", "could not save the configuration")
		}
		return
	}
	h.deps.Logger.Info("quality profile imported", "name", name, "app", plan.App, "formatsAdded", plan.Added())
	consoleJSON(w, http.StatusOK, map[string]any{"name": name, "formatsAdded": plan.Added()})
}

// plan builds the import preview for in, answering the request itself on
// failure.
func (h *consoleAPI) plan(w http.ResponseWriter, r *http.Request, in importRequest) (QualityImportPlan, bool) {
	app := strings.ToLower(strings.TrimSpace(in.App))
	if !validApp(app) {
		consoleError(w, http.StatusBadRequest, "invalid", "app must be radarr or sonarr")
		return QualityImportPlan{}, false
	}
	source := strings.ToLower(strings.TrimSpace(in.Source))
	if source != "" && source != ImportSourceTrash && source != ImportSourceDictionarry && source != ImportSourceProfilarr {
		consoleError(w, http.StatusBadRequest, "invalid", "source must be trash or dictionarry")
		return QualityImportPlan{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if source == ImportSourceDictionarry || source == ImportSourceProfilarr {
		if strings.TrimSpace(in.JSON) != "" || strings.TrimSpace(in.Profile) == "" {
			consoleError(w, http.StatusBadRequest, "invalid", "choose a Profilarr (Dictionarry) profile")
			return QualityImportPlan{}, false
		}
		imp, found, err := h.deps.Dictionarry.Import(ctx, app, in.Profile)
		if err != nil {
			h.dictionarryFail(w, err)
			return QualityImportPlan{}, false
		}
		cfg := config.Config{Resolver: h.s.resolverCfg(), Quality: h.s.qualityCfg()}
		return PlanQualityImport(imp, app, found, cfg), true
	}
	var data []byte
	switch {
	case strings.TrimSpace(in.JSON) != "" && in.Profile != "":
		consoleError(w, http.StatusBadRequest, "invalid", "send a TRaSH profile or pasted JSON, not both")
		return QualityImportPlan{}, false
	case strings.TrimSpace(in.JSON) != "":
		data = []byte(in.JSON)
	case in.Profile != "":
		var err error
		if data, err = h.deps.Guides.Profile(ctx, app, in.Profile); err != nil {
			h.importFail(w, err)
			return QualityImportPlan{}, false
		}
	default:
		consoleError(w, http.StatusBadRequest, "invalid", "choose a TRaSH profile or paste its JSON")
		return QualityImportPlan{}, false
	}
	imp, err := ImportQualityProfile(data)
	if err != nil {
		msg := strings.TrimPrefix(err.Error(), "quality profile: ")
		consoleError(w, http.StatusBadRequest, "invalid", strings.ToUpper(msg[:1])+msg[1:]+".")
		return QualityImportPlan{}, false
	}
	if imp.App != "" && imp.App != app && in.JSON != "" {
		app = imp.App // pasted: the quality names say which app it is for
	}
	// TRaSH's default-on format groups hold custom formats the profile's
	// formatItems don't name — the Base Profile names none of its own.
	extra, gerr := h.deps.Guides.ProfileGroupFormats(ctx, app, imp.TrashID)
	if len(extra) > 0 {
		if imp.FormatIDs == nil {
			imp.FormatIDs = map[string]string{}
		}
		for name, id := range extra {
			if _, ok := imp.FormatIDs[name]; !ok {
				imp.FormatIDs[name] = id
			}
		}
	}
	if gerr != nil && !errors.Is(gerr, ErrTrashNotFound) {
		imp.Warnings = append(imp.Warnings, fmt.Sprintf("Couldn't read TRaSH's default format groups (%s); only the profile's own formats are in this import.", trashReason(gerr)))
	}
	cfg := config.Config{Resolver: h.s.resolverCfg(), Quality: h.s.qualityCfg()}
	// Formats the owner already has are looked up too: TRaSH's current
	// scores for them come with the definition.
	found, ferr := h.deps.Guides.CustomFormats(ctx, app, imp.FormatIDs)
	plan := PlanQualityImport(imp, app, found, cfg)
	if ferr != nil {
		missing := 0
		for _, f := range plan.Formats {
			if f.Status == FormatMissing {
				missing++
			}
		}
		if missing > 0 {
			h.deps.Logger.Warn("quality profile import: custom formats unavailable", "err", ferr)
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("Couldn't look up every custom format (%s). Paste the missing ones under a quality profile's More options, then import again.", trashReason(ferr)))
			for i := range plan.Formats {
				if plan.Formats[i].Status == FormatMissing {
					plan.Formats[i].Reason = "Couldn't reach TRaSH Guides"
				}
			}
		}
	}
	return plan, true
}

func decodeImport(w http.ResponseWriter, r *http.Request, in *importRequest) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportBody))
	d.DisallowUnknownFields()
	if err := d.Decode(in); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			consoleError(w, http.StatusRequestEntityTooLarge, "too_large", "the profile JSON is larger than 1 MiB")
		} else {
			consoleError(w, http.StatusBadRequest, "invalid", "invalid request body")
		}
		return false
	}
	return true
}

// trashReason is err as the owner should read it.
func trashReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "GitHub did not answer in time"
	case errors.Is(err, ErrTrashUnavailable):
		return strings.TrimPrefix(err.Error(), ErrTrashUnavailable.Error()+": ")
	case errors.Is(err, ErrDictionarryUnavailable):
		return strings.TrimPrefix(err.Error(), ErrDictionarryUnavailable.Error()+": ")
	default:
		return err.Error()
	}
}

func (h *consoleAPI) importFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTrashNotFound):
		consoleError(w, http.StatusNotFound, "not_found", "TRaSH Guides has no such profile")
	case errors.Is(err, context.Canceled):
		consoleError(w, http.StatusBadRequest, "canceled", "the request was canceled")
	default:
		h.deps.Logger.Warn("TRaSH Guides request failed", "err", err)
		consoleError(w, http.StatusBadGateway, "upstream", "Couldn't load TRaSH Guides: "+trashReason(err)+".")
	}
}

func (h *consoleAPI) dictionarryFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrDictionarryNotFound):
		consoleError(w, http.StatusNotFound, "not_found", "Profilarr (Dictionarry) has no such profile")
	case errors.Is(err, context.Canceled):
		consoleError(w, http.StatusBadRequest, "canceled", "the request was canceled")
	default:
		h.deps.Logger.Warn("Profilarr (Dictionarry) request failed", "err", err)
		consoleError(w, http.StatusBadGateway, "upstream", "Couldn't load Profilarr (Dictionarry): "+trashReason(err)+".")
	}
}
