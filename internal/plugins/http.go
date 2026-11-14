package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/httpjson"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"github.com/braedonsaunders/ether/internal/viewspec"
)

// Handler serves the owner API. It is mounted at /api/plugins by the app,
// behind owner auth; paths here are relative to that mount.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", m.handleList)
	mux.HandleFunc("GET /catalogue", m.handleCatalogue)
	mux.HandleFunc("POST /catalogue/refresh", m.handleCatalogue)
	mux.HandleFunc("POST /install/preview", m.handlePreview)
	mux.HandleFunc("POST /install", m.handleInstall)
	mux.HandleFunc("GET /{id}", m.handleGet)
	mux.HandleFunc("GET /runtime-limits", m.handleRuntimeLimits)
	mux.HandleFunc("PUT /runtime-limits", m.handleRuntimeLimits)
	mux.HandleFunc("GET /{id}/jobs", m.handleActionJobs)
	mux.HandleFunc("POST /{id}/jobs/cancel", m.handleCancelActionJob)
	mux.HandleFunc("GET /{id}/resources/{resource}", m.handleStremioResource)
	mux.HandleFunc("GET /{id}/directories/{key}", m.handleDirectory)
	mux.HandleFunc("GET /{id}/settings", m.handleGetSettings)
	mux.HandleFunc("PUT /{id}/settings", m.handlePutSettings)
	mux.HandleFunc("GET /{id}/options/{field}", m.handleOptions)
	mux.HandleFunc("POST /{id}/test", m.handleTest)
	mux.HandleFunc("POST /{id}/enable", m.handleEnable)
	mux.HandleFunc("POST /{id}/disable", m.handleDisable)
	mux.HandleFunc("PUT /{id}/grants", m.handleGrants)
	mux.HandleFunc("GET /{id}/auto-update", m.handleGetAutoUpdate)
	mux.HandleFunc("PUT /{id}/auto-update", m.handlePutAutoUpdate)
	mux.HandleFunc("PUT /{id}/roles", m.handlePutRoles)
	mux.HandleFunc("POST /addons/preview", m.handleAddonPreview)
	mux.HandleFunc("POST /addons/configure-page", m.handleAddonLinkConfigPage)
	mux.HandleFunc("GET /{id}/addon-config", m.handleGetAddonConfig)
	mux.HandleFunc("POST /{id}/addon-config/test", m.handleTestAddonConfig)
	mux.HandleFunc("PUT /{id}/addon-config", m.handleSaveAddonConfig)
	mux.HandleFunc("POST /{id}/addon-config/page", m.handleAddonConfigPage)
	mux.HandleFunc("POST /addons", m.handleAddonInstall)
	mux.HandleFunc("POST /{id}/shadow", m.handleShadow)
	mux.HandleFunc("GET /{id}/shadow", m.handleShadowReport)
	mux.HandleFunc("GET /{id}/journal", m.handleJournal)
	mux.HandleFunc("POST /{id}/undo", m.handleUndo)
	mux.HandleFunc("GET /{id}/logs", m.handleLogs)
	mux.HandleFunc("GET /{id}/calls", m.handleCalls)
	mux.HandleFunc("POST /{id}/update/check", m.handleUpdateCheck)
	mux.HandleFunc("POST /{id}/update", m.handleUpdate)
	mux.HandleFunc("POST /{id}/rollback", m.handleRollback)
	mux.HandleFunc("DELETE /{id}", m.handleRemove)
	mux.HandleFunc("GET /{id}/views/{page}", m.handleView)
	mux.HandleFunc("POST /{id}/actions/{action}", m.handleAction)
	mux.HandleFunc("POST /{id}/proposals/{pid}", m.handleProposalDecision)
	return mux
}

func (m *Manager) handleCatalogue(w http.ResponseWriter, r *http.Request) {
	cat := m.getCatalogue()
	if cat == nil {
		writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}, "configured": false})
		return
	}
	cat.ServeHTTP(w, r)
}

// Summary is one row of the installed list.
type Summary struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	Kind          string `json:"kind,omitempty"`
	Summary       string `json:"summary,omitempty"`
	Icon          string `json:"icon,omitempty"`
	Enabled       bool   `json:"enabled"`
	Verified      bool   `json:"verified"`
	Shadow        bool   `json:"shadow"`
	AutoUpdate    bool   `json:"autoUpdate"`
	UpdateVersion string `json:"updateVersion,omitempty"`
	PrevVersion   string `json:"prevVersion,omitempty"`
	Health        string `json:"health"`
	// HealthReason is the plain-language reason behind a non-ok health,
	// such as "Recent calls were slow."
	HealthReason string `json:"healthReason,omitempty"`
	// HealthSidecar is "waiting" while the plugin's sidecar is not connected.
	HealthSidecar string   `json:"healthSidecar,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	SourceKind    string   `json:"sourceKind,omitempty"`
	StremioOnly   bool     `json:"stremioOnly,omitempty"`
	InstalledAt   int64    `json:"installedAt"`
	// Roles and Host describe a Stremio add-on: what it can do, which of
	// that is switched on, and the host it is reached at (never the link,
	// which carries the owner's configuration).
	Roles []RoleState `json:"roles,omitempty"`
	Host  string      `json:"host,omitempty"`
}

func (m *Manager) summary(r Record, h Health) Summary {
	s := Summary{ID: r.ID, Name: r.Name, Version: r.Version, Kind: r.Kind, Enabled: r.Enabled,
		Verified: r.Verified, Shadow: r.Shadow, AutoUpdate: r.AutoUpdate, UpdateVersion: r.UpdateVersion,
		PrevVersion: r.PrevVersion, Health: h.Status, HealthReason: h.Reason, HealthSidecar: h.Sidecar, SourceKind: r.SourceKind,
		StremioOnly: r.Manifest.IsStremioOnly(), InstalledAt: r.InstalledAt.Unix()}
	if r.Manifest.SpeaksStremio() {
		// Roles and the add-on's host belong to every plugin that answers
		// as an add-on, whether it came from a link or was edited in Studio.
		s.Roles = roleState(r)["roles"].([]RoleState)
		if u, err := url.Parse(r.SourceURL); err == nil {
			s.Host = displayHost(u.Hostname())
		}
	}
	if r.Manifest.IsStremioOnly() {
		s.Summary, s.Icon = r.Manifest.Description, r.Manifest.Logo
		for _, c := range r.Manifest.EffectiveEther().Capabilities {
			s.Capabilities = append(s.Capabilities, string(c))
		}
	}
	if r.Manifest.Ether != nil {
		s.Summary = r.Manifest.Ether.Summary
		if key, ok := strings.CutPrefix(s.Summary, "$t:"); ok {
			s.Summary = key
			if v, ok := m.ResolveText(context.Background(), r.ID, key, "en"); ok && strings.TrimSpace(v) != "" {
				s.Summary = v
			}
		}
		s.Icon = r.Manifest.Ether.Icon
		for _, c := range r.Manifest.Ether.Capabilities {
			s.Capabilities = append(s.Capabilities, string(c))
		}
	}
	// Plugin copy is often a "$t:" key; owners see the plugin's own text.
	if key, ok := strings.CutPrefix(s.Summary, "$t:"); ok {
		s.Summary = r.Manifest.Description
		if text, found := m.ResolveText(context.Background(), r.ID, key, "en"); found {
			s.Summary = text
		}
	}
	return s
}

// Detail is the plugin drawer payload: overview, permissions, health, the
// privacy label and impact.
type Detail struct {
	Summary
	Manifest      Manifest       `json:"manifest"`
	RuntimeLimits *RuntimeLimits `json:"runtimeLimits,omitempty"`
	Permissions   struct {
		Lines        []string    `json:"lines"`
		Declared     Permissions `json:"declared"`
		Grants       Grants      `json:"grants"`
		NeedsViewers bool        `json:"needsViewers"`
		NeedsFolders bool        `json:"needsFolders"`
	} `json:"permissions"`
	HealthDetail Health `json:"healthDetail"`
	Privacy      struct {
		DeclaredHosts []string       `json:"declaredHosts"`
		ObservedHosts map[string]int `json:"observedHosts"`
		Undeclared    []string       `json:"undeclared,omitempty"`
	} `json:"privacy"`
	Impact struct {
		Calls       int   `json:"calls"`
		Errors      int   `json:"errors"`
		P50MS       int64 `json:"p50Ms"`
		P95MS       int64 `json:"p95Ms"`
		EgressBytes int64 `json:"egressBytes"`
		KVBytes     int64 `json:"kvBytes"`
		Slow        int64 `json:"slow"`
		EventDrops  int64 `json:"eventDrops"`
		Restarts    int64 `json:"restarts"`
	} `json:"impact"`
	// AddedPermissions is set when an update is waiting and asks for more.
	AddedPermissions []string `json:"addedPermissions,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpjson.Write(w, status, v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string, fields []FieldError) {
	body := map[string]any{"error": map[string]any{"code": code, "message": msg}}
	if len(fields) > 0 {
		body["error"].(map[string]any)["fields"] = fields
	}
	writeJSON(w, status, body)
}

func (m *Manager) fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	var pie *PermissionIncreaseError
	var runtimeError *wasm.Error
	var input *CollectionInputError
	switch {
	case errors.As(err, &input):
		writeErr(w, http.StatusBadRequest, "validation", input.Message, nil)
	case errors.As(err, &runtimeError) && runtimeError.Status == wasm.StatusInvalidInput:
		writeErr(w, http.StatusBadRequest, "validation", runtimeError.Message, nil)
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "That plugin is not installed.", nil)
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, "validation", "Some fields need attention.", ve.Fields)
	case errors.As(err, &pie):
		fields := make([]FieldError, len(pie.Added))
		for i, a := range pie.Added {
			fields[i] = FieldError{"permissions", a}
		}
		writeErr(w, http.StatusConflict, "permission_increase", "This update asks for more access. Review it, then confirm.", fields)
	case errors.Is(err, ErrDigestMismatch):
		writeErr(w, http.StatusBadRequest, "digest_mismatch", "The downloaded package did not match its expected digest.", nil)
	case errors.Is(err, ErrRevoked):
		writeErr(w, http.StatusForbidden, "revoked", "This plugin has been revoked in the catalogue and cannot be enabled.", nil)
	case errors.Is(err, ErrPermissionDenied):
		writeErr(w, http.StatusForbidden, "permission_denied", "That access was not granted.", nil)
	case errors.Is(err, ErrNoRuntime):
		writeErr(w, http.StatusServiceUnavailable, "no_runtime", "This plugin cannot run yet because its runtime is not available.", nil)
	case errors.Is(err, ErrDisabled):
		writeErr(w, http.StatusConflict, "disabled", "This plugin is turned off.", nil)
	default:
		m.log.Warn("plugins api", "err", err)
		writeErr(w, http.StatusInternalServerError, "error", "Something went wrong. Details are in the server log.", nil)
	}
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close() //nolint:errcheck
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v)
}

func (m *Manager) handleList(w http.ResponseWriter, r *http.Request) {
	recs, err := m.store.list(r.Context())
	if err != nil {
		m.fail(w, err)
		return
	}
	out := make([]Summary, 0, len(recs))
	for _, rec := range recs {
		h, _ := m.store.health(r.Context(), rec.ID)
		out = append(out, m.summary(rec, m.liveHealth(rec, h)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": out})
}

func (m *Manager) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := m.store.record(r.Context(), id)
	if err != nil {
		m.fail(w, err)
		return
	}
	h, _ := m.store.health(r.Context(), id)
	h = m.liveHealth(rec, h)
	g, _ := m.store.grants(r.Context(), id)
	d := Detail{Summary: m.summary(rec, h), Manifest: rec.Manifest, RuntimeLimits: m.grantedRuntimeLimits(rec.Manifest), HealthDetail: h}
	d.Permissions.Lines, d.Permissions.NeedsViewers, d.Permissions.NeedsFolders = PermissionSheet(rec.Manifest)
	if rec.Manifest.Ether != nil {
		d.Permissions.Declared = rec.Manifest.Ether.Permissions
		d.Privacy.DeclaredHosts = rec.Manifest.Ether.Permissions.Network
	}
	d.Permissions.Grants = g
	d.Privacy.ObservedHosts = h.Observed
	for host := range h.Observed {
		if rec.Manifest.Ether == nil || !hostAllowed(rec.Manifest.Ether.Permissions.Network, host) {
			d.Privacy.Undeclared = append(d.Privacy.Undeclared, host)
		}
	}
	sort.Strings(d.Privacy.Undeclared)
	count, errs, p50, p95, _ := m.store.callStats(r.Context(), id)
	d.Impact.Calls, d.Impact.Errors, d.Impact.P50MS, d.Impact.P95MS = count, errs, p50, p95
	d.Impact.EgressBytes, d.Impact.Slow, d.Impact.EventDrops, d.Impact.Restarts = h.EgressBytes, h.Slow, h.Drops, h.Restarts
	d.Impact.KVBytes, _ = m.store.kvUsed(r.Context(), id)
	if rec.UpdateVersion != "" && rec.updateManifest != "" {
		if next, err := ParseManifest([]byte(rec.updateManifest)); err == nil {
			d.AddedPermissions = PermissionDiff(rec.Manifest, next)
		}
	}
	writeJSON(w, http.StatusOK, d)
}

type installRequest struct {
	ID                     string                     `json:"id,omitempty"`
	Version                string                     `json:"version,omitempty"`
	SHA256                 string                     `json:"sha256,omitempty"`
	URL                    string                     `json:"url"`
	Kind                   string                     `json:"kind"`
	LAN                    bool                       `json:"lan"`
	Manifest               json.RawMessage            `json:"manifest,omitempty"`
	Files                  map[string]json.RawMessage `json:"files,omitempty"`
	Grants                 Grants                     `json:"grants"`
	Settings               map[string]any             `json:"settings,omitempty"`
	AutoUpdate             bool                       `json:"autoUpdate"`
	Shadow                 bool                       `json:"shadow"`
	AcceptAddedPermissions bool                       `json:"acceptAddedPermissions"`
}

func (req installRequest) source() Source {
	src := Source{Kind: req.Kind, ID: req.ID, Version: req.Version, SHA256: req.SHA256, URL: req.URL, LAN: req.LAN}
	if len(req.Manifest) > 0 {
		src.Kind = "studio"
		src.Manifest = req.Manifest
		src.Files = map[string][]byte{}
		for k, v := range req.Files {
			var s string
			if json.Unmarshal(v, &s) == nil {
				src.Files[k] = []byte(s)
			} else {
				src.Files[k] = v
			}
		}
	}
	return src
}

func (m *Manager) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	// An uploaded .etherpkg arrives as the raw body.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/octet-stream") ||
		strings.HasPrefix(r.Header.Get("Content-Type"), "application/zip") {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			m.fail(w, err)
			return
		}
		p, err := m.PreviewInstall(r.Context(), Source{Kind: "pkg", Pkg: b})
		if err != nil {
			m.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	p, err := m.PreviewInstall(r.Context(), req.source())
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (m *Manager) handleInstall(w http.ResponseWriter, r *http.Request) {
	var src Source
	var opts InstallOptions
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", "The package could not be read.", nil)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("package")
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", "The package is missing.", nil)
			return
		}
		defer file.Close()
		b, err := io.ReadAll(file)
		if err != nil {
			m.fail(w, err)
			return
		}
		var req installRequest
		if err := json.Unmarshal([]byte(r.FormValue("options")), &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
			return
		}
		src = Source{Kind: "pkg", Pkg: b}
		opts = InstallOptions{Grants: req.Grants, Settings: req.Settings, AutoUpdate: req.AutoUpdate, Shadow: req.Shadow, AcceptAddedPermissions: req.AcceptAddedPermissions}
	} else if strings.HasPrefix(r.Header.Get("Content-Type"), "application/octet-stream") ||
		strings.HasPrefix(r.Header.Get("Content-Type"), "application/zip") {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			m.fail(w, err)
			return
		}
		src = Source{Kind: "pkg", Pkg: b}
	} else {
		var req installRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
			return
		}
		src = req.source()
		opts = InstallOptions{Grants: req.Grants, Settings: req.Settings, AutoUpdate: req.AutoUpdate, Shadow: req.Shadow, AcceptAddedPermissions: req.AcceptAddedPermissions}
	}
	p, err := m.PreviewInstall(r.Context(), src)
	if err != nil {
		m.fail(w, err)
		return
	}
	rec, err := m.Install(r.Context(), p, opts)
	if err != nil {
		m.fail(w, err)
		return
	}
	h, _ := m.store.health(r.Context(), rec.ID)
	writeJSON(w, http.StatusCreated, m.summary(rec, h))
}

func (m *Manager) handleGetAutoUpdate(w http.ResponseWriter, r *http.Request) {
	rec, err := m.store.record(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"autoUpdate": rec.AutoUpdate})
}

func (m *Manager) handlePutAutoUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoUpdate *bool `json:"autoUpdate"`
	}
	if err := decode(r, &body); err != nil || body.AutoUpdate == nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	rec, err := m.store.record(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, err)
		return
	}
	rec.AutoUpdate = *body.AutoUpdate
	if err := m.store.upsert(r.Context(), rec); err != nil {
		m.fail(w, err)
		return
	}
	m.broadcast(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"autoUpdate": rec.AutoUpdate})
}

// handlePutRoles switches an add-on's roles: {"off":["stream_source"]}.
func (m *Manager) handlePutRoles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Off []Capability `json:"off"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	rec, err := m.SetRoles(r.Context(), r.PathValue("id"), body.Off)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roleState(rec))
}

// AddonPreview is what the console shows before adding a Stremio add-on:
// the add-on, the roles it can play, and whether that link is already
// installed (adding it again only switches roles on).
type AddonPreview struct {
	ID               string      `json:"id"`
	Name             string      `json:"name"`
	Description      string      `json:"description,omitempty"`
	Logo             string      `json:"logo,omitempty"`
	Version          string      `json:"version,omitempty"`
	Host             string      `json:"host"`
	Roles            []RoleState `json:"roles"`
	Catalogs         []AddonCat  `json:"catalogs,omitempty"`
	AlreadyInstalled bool        `json:"alreadyInstalled,omitempty"`
	ConfigureFirst   bool        `json:"configureFirst,omitempty"`
}

// AddonCat is one catalog an add-on lists, for pickers.
type AddonCat struct {
	Key  string `json:"key"`
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

func addonCats(man Manifest) []AddonCat {
	var out []AddonCat
	for _, c := range man.Catalogs {
		name := c.Name
		if name == "" {
			name = c.ID
		}
		out = append(out, AddonCat{Key: c.Key(), Type: c.Type, ID: c.ID, Name: name})
	}
	return out
}

// handleAddonPreview reads an add-on link without installing it:
// {"url": "..."}.
func (m *Manager) handleAddonPreview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeErr(w, http.StatusBadRequest, "invalid", "Paste the plugin's install link.", nil)
		return
	}
	id, base, err := StremioInstallID(body.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "That is not a plugin install link. Paste the link its Stremio settings page gives you.", nil)
		return
	}
	out := AddonPreview{ID: id}
	if u, err := url.Parse(base); err == nil {
		out.Host = displayHost(u.Hostname())
	}
	if rec, err := m.Get(r.Context(), id); err == nil {
		out.AlreadyInstalled = true
		out.Name, out.Description, out.Logo, out.Version = rec.Name, rec.Manifest.Description, rec.Manifest.Logo, rec.Version
		out.Roles = roleState(rec)["roles"].([]RoleState)
		out.Catalogs = addonCats(rec.Manifest)
		writeJSON(w, http.StatusOK, out)
		return
	}
	p, err := m.PreviewInstall(r.Context(), Source{Kind: "url", URL: base, LAN: privateHost(base)})
	if err != nil {
		m.failAddon(w, err)
		return
	}
	if !p.Manifest.IsStremioOnly() {
		writeErr(w, http.StatusBadRequest, "invalid", "This link is an Ether plugin, not a Stremio one. Install it from Plugins.", nil)
		return
	}
	man := p.Manifest
	out.Name, out.Description, out.Logo, out.Version = man.Name, man.Description, man.Logo, man.Version
	out.ConfigureFirst = man.ConfigurationRequired()
	for _, c := range man.AddonRoles() {
		out.Roles = append(out.Roles, RoleState{Role: c, On: true})
	}
	out.Catalogs = addonCats(man)
	writeJSON(w, http.StatusOK, out)
}

// handleAddonInstall adds a Stremio add-on, or switches roles on for one
// already installed from the same link: {"url": "...", "roles": [...]}.
// An empty roles list switches every role the add-on offers on.
func (m *Manager) handleAddonInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL   string       `json:"url"`
		Roles []Capability `json:"roles"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.URL) == "" {
		writeErr(w, http.StatusBadRequest, "invalid", "Paste the plugin's install link.", nil)
		return
	}
	rec, err := m.InstallAddon(r.Context(), AddonInstall{Link: body.URL, Roles: body.Roles, Exclusive: len(body.Roles) > 0})
	if err != nil {
		m.failAddon(w, err)
		return
	}
	h, _ := m.store.health(r.Context(), rec.ID)
	writeJSON(w, http.StatusOK, m.summary(rec, h))
}

// failAddon reports an add-on link that could not be read in the owner's
// terms: the add-on is unreachable or did not answer with a manifest.
func (m *Manager) failAddon(w http.ResponseWriter, err error) {
	var ve *ValidationError
	if errors.As(err, &ve) || errors.Is(err, ErrNotFound) {
		m.fail(w, err)
		return
	}
	m.log.Info("add-on link not read", "err", err)
	writeErr(w, http.StatusBadGateway, "unreachable", "Ether couldn't read a plugin at that link. Check the link, and that the plugin is online and allows servers.", nil)
}

// RoleState is one role of an add-on for the console.
type RoleState struct {
	Role Capability `json:"role"`
	On   bool       `json:"on"`
}

func roleState(rec Record) map[string]any {
	off := map[Capability]bool{}
	for _, c := range rec.RolesOff {
		off[c] = true
	}
	roles := []RoleState{}
	for _, c := range rec.Manifest.AddonRoles() {
		roles = append(roles, RoleState{Role: c, On: !off[c]})
	}
	return map[string]any{"roles": roles}
}

func (m *Manager) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, schema, err := m.GetSettings(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, err)
		return
	}
	if len(schema) == 0 {
		schema = json.RawMessage("{}")
	}
	// Field labels and hints use the plugin's own "$t:" strings.
	if walk := m.textResolver(r.Context(), r.PathValue("id"), r.URL.Query().Get("locale")); walk != nil {
		var node any
		if json.Unmarshal(schema, &node) == nil {
			if out, err := json.Marshal(walk(node)); err == nil {
				schema = out
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "schema": schema})
}

func (m *Manager) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	fieldErrors, err := m.Configure(r.Context(), r.PathValue("id"), body.Settings)
	if err != nil {
		m.fail(w, err)
		return
	}
	if len(fieldErrors) > 0 {
		fields := make([]FieldError, 0, len(fieldErrors))
		for k, v := range fieldErrors {
			fields = append(fields, FieldError{k, v})
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].Field < fields[j].Field })
		writeErr(w, http.StatusBadRequest, "validation", "The plugin did not accept these settings.", fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved"})
}

func (m *Manager) handleOptions(w http.ResponseWriter, r *http.Request) {
	body, err := m.Options(r.Context(), r.PathValue("id"), r.PathValue("field"), map[string]string{"q": r.URL.Query().Get("q"), "locale": r.URL.Query().Get("locale")})
	if err != nil {
		m.fail(w, err)
		return
	}
	if len(body) == 0 {
		body = json.RawMessage("[]")
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": body})
}

func (m *Manager) handleTest(w http.ResponseWriter, r *http.Request) {
	out, err := m.Test(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *Manager) handleEnable(w http.ResponseWriter, r *http.Request) {
	if err := m.SetEnabled(r.Context(), r.PathValue("id"), true); err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "enabled"})
}

func (m *Manager) handleDisable(w http.ResponseWriter, r *http.Request) {
	if err := m.SetEnabled(r.Context(), r.PathValue("id"), false); err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "disabled"})
}

func (m *Manager) handleGrants(w http.ResponseWriter, r *http.Request) {
	var g Grants
	if err := decode(r, &g); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	if err := m.SetGrants(r.Context(), r.PathValue("id"), g); err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "grants": g})
}

func (m *Manager) handleShadow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Shadow bool `json:"shadow"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid", "The request was not understood.", nil)
		return
	}
	if err := m.SetShadow(r.Context(), r.PathValue("id"), body.Shadow); err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shadow": body.Shadow})
}

// ShadowReport lists what a shadow-mode plugin would have done.
func (m *Manager) handleShadowReport(w http.ResponseWriter, r *http.Request) {
	entries, err := m.Journal(r.Context(), r.PathValue("id"), 200, true)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "count": len(entries)})
}

// Journal returns a plugin's journal (liveOnly: applied and not undone;
// shadowOnly is handled by the caller through Journal).
func (m *Manager) Journal(ctx context.Context, id string, limit int, shadowOnly bool) ([]JournalEntry, error) {
	if _, err := m.store.record(ctx, id); err != nil {
		return nil, err
	}
	entries, err := m.store.journalList(ctx, id, limit, false)
	if err != nil || !shadowOnly {
		return entries, err
	}
	out := entries[:0:0]
	for _, e := range entries {
		if e.Shadow {
			out = append(out, e)
		}
	}
	return out, nil
}

func limitOf(r *http.Request, def int) int {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			return n
		}
	}
	return def
}

func (m *Manager) handleJournal(w http.ResponseWriter, r *http.Request) {
	entries, err := m.Journal(r.Context(), r.PathValue("id"), limitOf(r, 200), r.URL.Query().Get("shadow") == "1")
	if err != nil {
		m.fail(w, err)
		return
	}
	if entries == nil {
		entries = []JournalEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (m *Manager) handleUndo(w http.ResponseWriter, r *http.Request) {
	// ?since=<entry id> undoes back to that change; no value undoes all.
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	n, err := m.UndoSince(r.Context(), r.PathValue("id"), since)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"undone": n})
}

func (m *Manager) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := m.store.record(r.Context(), id); err != nil {
		m.fail(w, err)
		return
	}
	lines, err := m.store.logs(r.Context(), id, limitOf(r, LogsCap))
	if err != nil {
		m.fail(w, err)
		return
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (m *Manager) handleCalls(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := m.store.record(r.Context(), id); err != nil {
		m.fail(w, err)
		return
	}
	calls, err := m.store.calls(r.Context(), id, limitOf(r, CallsCap))
	if err != nil {
		m.fail(w, err)
		return
	}
	if calls == nil {
		calls = []CallRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": calls})
}

func (m *Manager) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	p, available, err := m.CheckUpdate(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updateAvailable": available, "version": p.Manifest.Version,
		"addedPermissions": p.AddedPermissions,
	})
}

func (m *Manager) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AcceptAddedPermissions bool `json:"acceptAddedPermissions"`
	}
	_ = decode(r, &body)
	rec, err := m.ApplyUpdate(r.Context(), r.PathValue("id"), body.AcceptAddedPermissions)
	if err != nil {
		m.fail(w, err)
		return
	}
	h, _ := m.store.health(r.Context(), rec.ID)
	writeJSON(w, http.StatusOK, m.summary(rec, h))
}

func (m *Manager) handleRollback(w http.ResponseWriter, r *http.Request) {
	rec, err := m.Rollback(r.Context(), r.PathValue("id"))
	if err != nil {
		m.fail(w, err)
		return
	}
	h, _ := m.store.health(r.Context(), rec.ID)
	writeJSON(w, http.StatusOK, m.summary(rec, h))
}

func (m *Manager) handleRemove(w http.ResponseWriter, r *http.Request) {
	undo := r.URL.Query().Get("undo") == "1"
	if err := m.Remove(r.Context(), r.PathValue("id"), undo); err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "removed"})
}

// handleView relays a plugin page request (SPEC §5.6): WS9j and WS10
// render the {spec,data} the plugin returns.
func (m *Manager) handleView(w http.ResponseWriter, r *http.Request) {
	id, page := r.PathValue("id"), r.PathValue("page")
	rec, err := m.store.record(r.Context(), id)
	if err != nil {
		m.fail(w, err)
		return
	}
	route := ""
	if rec.Manifest.Ether != nil {
		for _, p := range rec.Manifest.Ether.Contributes.Pages {
			if p.Key == page {
				route = p.Route
			}
		}
		if route == "" {
			for _, slot := range rec.Manifest.Ether.Contributes.Slots {
				if slot.Key == page {
					route = slot.Route
					break
				}
			}
		}
	}
	if route == "" {
		writeErr(w, http.StatusNotFound, "not_found", "That page does not exist.", nil)
		return
	}
	query := map[string]string{}
	for k := range r.URL.Query() {
		query[k] = r.URL.Query().Get(k)
	}
	resp, err := m.Call(r.Context(), id, Request{Method: "GET", Path: route, Query: query, Locale: query["locale"]})
	if err != nil {
		m.fail(w, err)
		return
	}
	if resp.Status >= 200 && resp.Status < 300 {
		var doc struct {
			Spec json.RawMessage `json:"spec"`
			Data json.RawMessage `json:"data"`
		}
		if len(resp.Body) > 1<<20 || json.Unmarshal(resp.Body, &doc) != nil || !viewspec.ValidatePlugin(doc.Spec).OK {
			writeErr(w, http.StatusBadGateway, "invalid_view", "Plugin returned an invalid view.", nil)
			return
		}
	}
	if resp.Status >= 200 && resp.Status < 300 {
		resp.Body = m.localizeView(r.Context(), id, query["locale"], resp.Body)
	}
	clean := m.store.redactKnown(r.Context(), id, string(resp.Body))
	if !json.Valid([]byte(clean)) {
		writeErr(w, http.StatusBadGateway, "invalid_view", "Plugin returned an invalid view.", nil)
		return
	}
	resp.Body = []byte(clean)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// textResolver returns a walker that replaces "$t:key" strings with the
// plugin's shipped text for locale (then its language, then English), or
// nil when the plugin ships no strings.
func (m *Manager) textResolver(ctx context.Context, id, locale string) func(any) any {
	row, err := m.store.settings(ctx, id)
	if err != nil || len(row.Strings) == 0 {
		return nil
	}
	resolve := func(key string) (string, bool) {
		for _, loc := range []string{locale, strings.SplitN(locale, "-", 2)[0], "en"} {
			if s, ok := row.Strings[loc][key]; ok && loc != "" {
				return s, true
			}
		}
		return "", false
	}
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			if key, ok := strings.CutPrefix(t, "$t:"); ok {
				if s, found := resolve(key); found {
					return s
				}
			}
		case map[string]any:
			for k, child := range t {
				t[k] = walk(child)
			}
		case []any:
			for i, child := range t {
				t[i] = walk(child)
			}
		}
		return v
	}
	return walk
}

// localizeView resolves "$t:key" references in a view's spec and title from
// the plugin's shipped strings, the way Studio's preview does. View data is
// plugin-supplied values and is left as sent.
func (m *Manager) localizeView(ctx context.Context, id, locale string, body []byte) []byte {
	walk := m.textResolver(ctx, id, locale)
	if walk == nil {
		return body
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	for _, field := range []string{"spec", "title"} {
		raw, ok := doc[field]
		if !ok {
			continue
		}
		var node any
		if json.Unmarshal(raw, &node) != nil {
			continue
		}
		if out, err := json.Marshal(walk(node)); err == nil {
			doc[field] = out
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

func (m *Manager) handleAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		m.fail(w, err)
		return
	}
	if len(body) > 1<<20 {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "Action request is too large.", nil)
		return
	}
	resp, err := m.Call(r.Context(), id, Request{Method: "POST", Path: "/ether/action/" + action, Body: body})
	if err != nil {
		m.fail(w, err)
		return
	}
	if resp.Status < 200 || resp.Status >= 300 {
		clean := m.store.redactKnown(r.Context(), id, string(resp.Body))
		if !json.Valid([]byte(clean)) {
			writeErr(w, http.StatusBadGateway, "invalid_action", "Plugin returned an invalid action.", nil)
			return
		}
		resp.Body = []byte(clean)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
		return
	}
	if err := m.startActionJob(id, resp.Body); err != nil {
		m.fail(w, err)
		return
	}
	result, err := m.saveActionProposal(r.Context(), id, resp.Body)
	if err != nil {
		m.fail(w, err)
		return
	}
	clean := m.store.redactKnown(r.Context(), id, string(result))
	if !json.Valid([]byte(clean)) {
		writeErr(w, http.StatusBadGateway, "invalid_action", "Plugin returned an invalid action.", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(clean))
}
