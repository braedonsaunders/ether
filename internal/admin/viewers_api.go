package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Personalization is the engine behind the viewers, Home Designer and
// insights endpoints. Implemented by internal/personalize.Engine.
type Personalization interface {
	ViewerStats(ctx context.Context, userIDs []int64) (map[int64]core.ViewerStats, error)
	RecentActivity(ctx context.Context, userID int64, limit int) ([]core.ViewerActivity, error)
	Taste(ctx context.Context, user *core.User) (*core.TasteProfile, error)
	SetTasteOverrides(ctx context.Context, user *core.User, overrides []core.TasteOverride) (*core.TasteProfile, error)
	// PreviewHome is exactly what /hubs (or /hubs/sections/{id} when
	// section is "movies", "shows" or a section id) serves the viewer in
	// the context.
	PreviewHome(ctx context.Context, user *core.User, hc core.HubContext, section string, limit int) (*core.HomePreview, error)
	Refresh(ctx context.Context, user *core.User) error
	// HomeLayout is the saved Home layout document; SetHomeLayout
	// validates and replaces it, applying from the next /hubs request.
	HomeLayout(ctx context.Context) (core.HomeLayout, error)
	SetHomeLayout(ctx context.Context, d core.HomeLayout) (core.HomeLayout, error)
	// PreviewLayout assembles what the viewer would see under a draft
	// document without saving it.
	PreviewLayout(ctx context.Context, user *core.User, hc core.HubContext, section string, limit int, d core.HomeLayout) (*core.HomePreview, error)
	// LayoutDefaults is the default row count per section (home, movies,
	// shows).
	LayoutDefaults() map[string]int
	AvailableRows(ctx context.Context) ([]core.RowDescriptor, error)
	RowInsights(ctx context.Context, days int) ([]core.RowInsight, error)
	// RankerInsight describes the learned ranking model.
	RankerInsight(ctx context.Context) (core.RankerInsight, error)
	Activity(ctx context.Context, days int) ([]core.ActivityDay, error)
}

// invalidFields is implemented by Personalization validation errors.
type invalidFields interface {
	error
	InvalidFields() [][2]string
}

// personalizationRoutes registers the viewers, layout and insights API.
func (h *Handler) personalizationRoutes(api func(pattern string, fn http.HandlerFunc)) {
	api("GET /api/viewers", h.handleViewers)
	api("GET /api/viewers/{id}", h.handleViewer)
	api("GET /api/viewers/{id}/taste", h.handleViewerTaste)
	api("PUT /api/viewers/{id}/taste/overrides", h.handleViewerOverrides)
	api("GET /api/viewers/{id}/home", h.handleViewerHome)
	api("POST /api/viewers/{id}/refresh", h.handleViewerRefresh)
	api("GET /api/layout", h.handleGetLayout)
	api("PUT /api/layout", h.handlePutLayout)
	api("POST /api/layout/preview", h.handleLayoutPreview)
	api("POST /api/layout/impact", h.handleLayoutImpact)
	api("GET /api/insights/rows", h.handleInsightRows)
	api("GET /api/insights/ranker", h.handleInsightRanker)
	api("GET /api/insights/activity", h.handleInsightActivity)

	api("GET /api/viewers/{id}/plex-watch", h.handlePlexWatchStatus)
	api("POST /api/viewers/{id}/plex-watch/pin", h.handlePlexWatchPIN)
	api("POST /api/viewers/{id}/plex-watch/pin/finish", h.handlePlexWatchPINFinish)
	api("PUT /api/viewers/{id}/plex-watch", h.handlePlexWatchEnabled)
	api("POST /api/viewers/{id}/plex-watch/disconnect", h.handlePlexWatchDisconnect)
	api("POST /api/viewers/{id}/plex-watch/retry", h.handlePlexWatchRetry)
	api("POST /api/viewers/{id}/plex-watch/export", h.handlePlexWatchExport)
	api("POST /api/viewers/{id}/plex-watch/import", h.handlePlexWatchImport)
}

// writeEngineError maps engine errors onto the API's error shapes.
func writeEngineError(w http.ResponseWriter, err error) {
	var inv invalidFields
	switch {
	case errors.As(err, &inv):
		var fields []FieldError
		for _, f := range inv.InvalidFields() {
			fields = append(fields, FieldError{Field: f[0], Message: f[1]})
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": "validation", "message": inv.Error(), "fields": fields},
		})
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "the personalization engine did not answer in time")
	default:
		writeError(w, http.StatusInternalServerError, "personalization_failed", err.Error())
	}
}

// ViewerRecord is a stored identity plus read-only Plex membership metadata.
// Imported history does not grant server access.
type ViewerRecord struct {
	core.User
	Active bool
	Origin string
}

func (u ViewerRecord) accessStatus() string {
	if u.IsOwner {
		return "owner"
	}
	if u.Active {
		return "active"
	}
	if u.Origin == "imported" {
		return "imported"
	}
	return "inactive"
}

// viewer is the Viewer shape of the contract.
type viewer struct {
	ID              int64      `json:"id"`
	PlexID          int64      `json:"plexId"`
	Username        string     `json:"username"`
	Title           string     `json:"title"`
	Thumb           string     `json:"thumb,omitempty"`
	IsOwner         bool       `json:"isOwner"`
	AccessStatus    string     `json:"accessStatus"`
	LastActive      *time.Time `json:"lastActive,omitempty"`
	Plays30d        int        `json:"plays30d"`
	HoursWatched30d float64    `json:"hoursWatched30d"`
}

func toViewer(u ViewerRecord, st core.ViewerStats) viewer {
	v := viewer{ID: u.ID, PlexID: u.PlexID, Username: u.Username, Title: u.Title, Thumb: u.Thumb, IsOwner: u.IsOwner,
		AccessStatus: u.accessStatus(), Plays30d: st.Plays30d, HoursWatched30d: st.HoursWatched30d}
	if v.Title == "" {
		v.Title = u.Username
	}
	if !st.LastActive.IsZero() {
		t := st.LastActive.UTC()
		v.LastActive = &t
	}
	return v
}

// viewerByID looks a user up for a {id} path parameter, answering 400/404
// itself when it cannot.
func (h *Handler) viewerByID(w http.ResponseWriter, r *http.Request) (*ViewerRecord, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "viewer id must be a number")
		return nil, false
	}
	users, err := h.storedViewers(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "users_failed", err.Error())
		return nil, false
	}
	for i := range users {
		if users[i].ID == id {
			u := users[i]
			return &u, true
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "no such viewer")
	return nil, false
}

func (h *Handler) handleViewers(w http.ResponseWriter, r *http.Request) {
	users, err := h.storedViewers(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "users_failed", err.Error())
		return
	}
	ids := make([]int64, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	stats, err := h.deps.Personalization.ViewerStats(r.Context(), ids)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	out := make([]viewer, 0, len(users))
	for _, u := range users {
		out = append(out, toViewer(u, stats[u.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewers": out})
}

type recentViewing struct {
	Title     homeTitleCard `json:"title"`
	At        time.Time     `json:"at"`
	Completed bool          `json:"completed"`
	Progress  float64       `json:"progress"`
}

func (h *Handler) handleViewer(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewerByID(w, r)
	if !ok {
		return
	}
	stats, err := h.deps.Personalization.ViewerStats(r.Context(), []int64{u.ID})
	if err != nil {
		writeEngineError(w, err)
		return
	}
	recent, err := h.deps.Personalization.RecentActivity(r.Context(), u.ID, 30)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	rv := make([]recentViewing, 0, len(recent))
	for _, a := range recent {
		rv = append(rv, recentViewing{Title: homeCard(a.Title), At: a.At.UTC(), Completed: a.Completed, Progress: a.Progress})
	}
	writeJSON(w, http.StatusOK, struct {
		viewer
		Recent []recentViewing `json:"recent"`
	}{toViewer(*u, stats[u.ID]), rv})
}

func (h *Handler) handleViewerTaste(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewerByID(w, r)
	if !ok {
		return
	}
	p, err := h.deps.Personalization.Taste(r.Context(), &u.User)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) handleViewerOverrides(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewerByID(w, r)
	if !ok {
		return
	}
	var body struct {
		Overrides []core.TasteOverride `json:"overrides"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Overrides == nil {
		body.Overrides = []core.TasteOverride{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	p, err := h.deps.Personalization.SetTasteOverrides(ctx, &u.User, body.Overrides)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// homeRow is the HomeRow shape of the contract.
type homeRow struct {
	HubID        string            `json:"hubId"`
	Layer        string            `json:"layer,omitempty"`
	Slot         int               `json:"slot,omitempty"`
	AnchorSlot   int               `json:"anchorSlot,omitempty"`
	Placement    string            `json:"placement,omitempty"`
	HiddenReason string            `json:"hiddenReason,omitempty"`
	Policy       *core.RowPolicy   `json:"policy,omitempty"`
	PolicyLayer  string            `json:"policyLayer,omitempty"`
	Title        string            `json:"title"`
	Kind         string            `json:"kind"`
	Reason       string            `json:"reason"`
	Score        float64           `json:"score"`
	Pinned       bool              `json:"pinned,omitempty"`
	Hidden       bool              `json:"hidden,omitempty"`
	Source       string            `json:"source"`
	Items        []homeTitleCard   `json:"items"`
	ItemReasons  map[string]string `json:"itemReasons,omitempty"`
}

func (h *Handler) handleViewerHome(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewerByID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	device := strings.ToLower(strings.TrimSpace(q.Get("device")))
	if device == "" {
		device = core.DeviceTV
	}
	switch device {
	case core.DeviceTV, core.DeviceMobile, core.DeviceWeb, core.DeviceDesktop:
	default:
		writeValidationError(w, []FieldError{{Field: "device", Message: "must be tv, mobile, web or desktop"}})
		return
	}
	at := time.Now()
	if v := q.Get("at"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeValidationError(w, []FieldError{{Field: "at", Message: "must be an RFC 3339 time"}})
			return
		}
		at = t
	}
	limit := 12
	if v := q.Get("count"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeValidationError(w, []FieldError{{Field: "count", Message: "must be 1-100"}})
			return
		}
		limit = n
	}
	hc := core.NewHubContext(at.Local(), "", "", "")
	hc.Device = device
	p, err := h.deps.Personalization.PreviewHome(r.Context(), &u.User, hc, q.Get("section"), limit)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	h.writeHomePreview(w, p, nil)
}

func (h *Handler) writeHomePreview(w http.ResponseWriter, p *core.HomePreview, viewer any) {
	rows := make([]homeRow, 0, len(p.Rows))
	for _, row := range p.Rows {
		hr := homeRow{HubID: row.HubID, Title: row.Title, Kind: row.Kind, Reason: row.Reason, Score: row.Score,
			Layer: row.Layer, Slot: row.Slot, AnchorSlot: row.AnchorSlot, Placement: row.Placement, HiddenReason: row.HiddenReason, Policy: row.Policy, PolicyLayer: row.PolicyLayer, Pinned: row.Pinned, Hidden: row.Hidden, Source: row.Source, Items: make([]homeTitleCard, 0, len(row.Items))}
		for _, t := range row.Items {
			hr.Items = append(hr.Items, homeCard(t))
		}
		if len(row.ItemReasons) > 0 {
			hr.ItemReasons = make(map[string]string, len(row.ItemReasons))
			for k, v := range row.ItemReasons {
				hr.ItemReasons[k.String()] = v
			}
		}
		rows = append(rows, hr)
	}
	body := map[string]any{
		"rows":        rows,
		"viewer":      viewer,
		"generatedAt": p.GeneratedAt.UTC(),
		"context":     map[string]any{"device": p.Context.Device, "daypart": p.Context.Daypart, "weekend": p.Context.Weekend},
	}
	if p.HomeDeduped > 0 {
		// Titles kept off a Movies/TV Shows page because the Home leads
		// with them (DedupeHome).
		body["homeDeduped"] = p.HomeDeduped
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *Handler) handleViewerRefresh(w http.ResponseWriter, r *http.Request) {
	u, ok := h.viewerByID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := h.deps.Personalization.Refresh(ctx, &u.User); err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- TitleCard -----------------------------------------------------------

// homeTitleCard is the TitleCard shape of the contract.
type homeTitleCard struct {
	Key            string   `json:"key"`
	Type           string   `json:"type"`
	Title          string   `json:"title"`
	Year           int      `json:"year,omitempty"`
	Summary        string   `json:"summary,omitempty"`
	Poster         string   `json:"poster,omitempty"`
	Backdrop       string   `json:"backdrop,omitempty"`
	Rating         float64  `json:"rating,omitempty"`
	ContentRating  string   `json:"contentRating,omitempty"`
	RuntimeMinutes int      `json:"runtimeMinutes,omitempty"`
	Genres         []string `json:"genres,omitempty"`
	ShowTitle      string   `json:"showTitle,omitempty"`
	Season         int      `json:"season,omitempty"`
	Episode        int      `json:"episode,omitempty"`
}

// adminImageURL is a browser-loadable URL for a TMDB image path, served
// by the admin image proxy.
func adminImageURL(path string, width int) string {
	if path == "" {
		return ""
	}
	return "/ether/api/image?path=" + url.QueryEscape(path) + "&w=" + strconv.Itoa(width)
}

func homeCard(t core.Title) homeTitleCard {
	c := homeTitleCard{
		Key: t.Key.String(), Type: string(t.Type), Title: t.Title, Year: t.Year, Summary: t.Summary,
		Poster: adminImageURL(t.PosterPath, 342), Backdrop: adminImageURL(t.BackdropPath, 780),
		Rating: t.Rating, ContentRating: t.ContentRating, RuntimeMinutes: int(t.Runtime.Minutes()),
		Genres: t.Genres, ShowTitle: t.ShowTitle, Season: t.Season, Episode: t.Episode,
	}
	if c.Backdrop == "" && t.StillPath != "" {
		c.Backdrop = adminImageURL(t.StillPath, 780)
	}
	return c
}

// viewerDirectory is owner-facing identity/history browsing, not membership.
func (h *Handler) viewerDirectory(ctx context.Context) ([]core.User, error) {
	stored, err := h.storedViewers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.User, 0, len(stored))
	for _, u := range stored {
		out = append(out, u.User)
	}
	return out, nil
}

func (h *Handler) storedViewers(ctx context.Context) ([]ViewerRecord, error) {
	if h.deps.ViewerDirectory != nil {
		return h.deps.ViewerDirectory(ctx)
	}
	users, err := h.deps.Users.Users(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ViewerRecord, 0, len(users))
	for _, u := range users {
		out = append(out, ViewerRecord{User: u, Active: true})
	}
	return out, nil
}
