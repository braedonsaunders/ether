package app

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/resolver"
)

// qualityClient is a known app the owner can ask "who gets what" about.
// The identities are what each official app sends, so the capability
// registry and quality rules match them exactly as they match playback.
type qualityClient struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Product  string        `json:"product"`
	Platform string        `json:"platform"`
	Device   string        `json:"device,omitempty"`
	Model    string        `json:"model,omitempty"`
	Protocol core.Protocol `json:"protocol,omitempty"`
}

func qualityClients() []qualityClient {
	return []qualityClient{
		{ID: "plex-apple-tv", Name: "Plex for Apple TV", Product: "Plex for Apple TV", Platform: "tvOS", Device: "Apple TV"},
		{ID: "plex-ios", Name: "Plex for iPhone and iPad", Product: "Plex for iOS", Platform: "iOS", Device: "iPhone"},
		{ID: "plex-android-tv", Name: "Plex for Android TV", Product: "Plex for Android (TV)", Platform: "Android", Device: "Android TV"},
		{ID: "plex-shield", Name: "Plex on NVIDIA Shield", Product: "Plex for Android (TV)", Platform: "Android", Device: "SHIELD Android TV"},
		{ID: "plex-fire-tv", Name: "Plex for Fire TV", Product: "Plex for Android (TV)", Platform: "Android", Device: "AFTKRT"},
		{ID: "plex-android", Name: "Plex for Android phones", Product: "Plex for Android (Mobile)", Platform: "Android", Device: "Pixel 8"},
		{ID: "plex-roku", Name: "Plex for Roku", Product: "Plex for Roku", Platform: "Roku"},
		{ID: "plex-lg", Name: "Plex for LG", Product: "Plex for LG", Platform: "webOS"},
		{ID: "plex-samsung", Name: "Plex for Samsung", Product: "Plex for Samsung", Platform: "Tizen"},
		{ID: "plex-xbox", Name: "Plex for Xbox", Product: "Plex for Xbox", Platform: "Xbox"},
		{ID: "plex-playstation", Name: "Plex for PlayStation", Product: "Plex for PlayStation", Platform: "PlayStation"},
		{ID: "plex-web-chrome", Name: "Plex Web in Chrome", Product: "Plex Web", Platform: "Chrome"},
		{ID: "plex-web-safari", Name: "Plex Web in Safari", Product: "Plex Web", Platform: "Safari"},
		{ID: "plex-htpc", Name: "Plex HTPC", Product: "Plex HTPC", Platform: "Windows"},
		{ID: "plex-kodi", Name: "Plex for Kodi", Product: "PM4K", Device: "Kodi"},
		{ID: "plex-desktop", Name: "Plex for Windows and Mac", Product: "Plex for Windows", Platform: "Windows"},
		{ID: "jellyfin", Name: "Jellyfin", Product: "Jellyfin", Protocol: "jellyfin"},
		{ID: "emby", Name: "Emby", Product: "Emby", Protocol: "emby"},
	}
}

type qualityDeviceApp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	// Devices are the registry device types the app runs on.
	Devices []string `json:"devices,omitempty"`
	// Aliases are the devices the app runs on and their other names, so
	// searching "iPad" or "Shield" finds the app.
	Aliases []string `json:"aliases,omitempty"`
}

// qualityAppAliases are the search words for a registry app: the device
// types it runs on and their aliases.
func qualityAppAliases(app clientreg.App) []string {
	var out []string
	add := func(s string) {
		if s != "" && s != app.Name && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, d := range app.Devices {
		add(d)
		for _, alias := range clientreg.DeviceAliases(d) {
			add(alias)
		}
	}
	return out
}

type qualityDeviceClient struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Group    string    `json:"group,omitempty"`
	Type     string    `json:"type,omitempty"`
	App      string    `json:"app,omitempty"`
	Viewer   string    `json:"viewer,omitempty"`
	LastSeen time.Time `json:"lastSeen"`
}

type qualityDevicesResponse struct {
	Groups  []clientreg.ClassInfo      `json:"groups"`
	Types   []clientreg.DeviceTypeInfo `json:"types"`
	Apps    []qualityDeviceApp         `json:"apps"`
	Clients []qualityDeviceClient      `json:"clients"`
}

type qualityExplainIn struct {
	UserID    int64          `json:"userId"`
	ClientID  string         `json:"clientId,omitempty"`
	Client    *qualityClient `json:"client,omitempty"`
	Remote    bool           `json:"remote"`
	TitleKey  string         `json:"titleKey,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	LibraryID int64          `json:"libraryId,omitempty"`
}

// qualityHandler serves the Releases page's device picker, summary table and
// "Check one play" explainer.
func (a *App) qualityHandler() http.Handler {
	m := &route.Mux{}
	m.HandleFunc("GET /clients", func(w http.ResponseWriter, r *http.Request) {
		writeQualityJSON(w, http.StatusOK, map[string]any{"clients": qualityClients()})
	})
	m.HandleFunc("GET /devices", a.handleQualityDevices)
	m.HandleFunc("GET /summary", a.handleQualitySummary)
	m.HandleFunc("POST /explain", a.handleQualityExplain)
	return m
}

func (a *App) handleQualityDevices(w http.ResponseWriter, r *http.Request) {
	groups := clientreg.Classes()
	types := clientreg.DeviceTypes()
	regApps := clientreg.Apps()
	apps := make([]qualityDeviceApp, 0, len(regApps))
	for _, item := range regApps {
		apps = append(apps, qualityDeviceApp{
			ID:       clientreg.AppID(item),
			Name:     item.Name,
			Protocol: item.Protocol,
			Devices:  slices.Clone(item.Devices),
			Aliases:  qualityAppAliases(item),
		})
	}
	clients := []qualityDeviceClient{}
	if a.db != nil {
		if seen, err := a.seenPlaybackDevices(r.Context()); err == nil {
			clients = make([]qualityDeviceClient, 0, len(seen))
			for _, d := range seen {
				id := clientreg.Identity{
					Product:    d.App,
					Platform:   d.Platform,
					Device:     d.Device,
					DeviceName: d.Name,
				}
				name := strings.TrimSpace(d.Name)
				if name == "" {
					name = strings.TrimSpace(d.App)
				}
				if name == "" {
					name = strings.TrimSpace(d.Device)
				}
				if name == "" {
					name = d.ClientIdentifier
				}
				clients = append(clients, qualityDeviceClient{
					ID:       d.ClientIdentifier,
					Name:     name,
					Group:    clientreg.Class(id),
					Type:     clientreg.DeviceType(id),
					App:      clientreg.AppFor("", id),
					Viewer:   d.Viewer,
					LastSeen: d.LastSeen,
				})
			}
		}
	}
	writeQualityJSON(w, http.StatusOK, qualityDevicesResponse{
		Groups:  groups,
		Types:   types,
		Apps:    apps,
		Clients: clients,
	})
}

func (a *App) pluginDisplayName(ctx context.Context, pluginID string) string {
	if pluginID == "" {
		return ""
	}
	if a.pluginsRT != nil && a.pluginsRT.mgr != nil {
		if rec, err := a.pluginsRT.mgr.Get(ctx, pluginID); err == nil && strings.TrimSpace(rec.Name) != "" {
			return rec.Name
		}
	}
	return pluginID
}

func (a *App) handleQualitySummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var viewerID int64
	if rawViewer := strings.TrimSpace(q.Get("viewer")); rawViewer != "" {
		id, err := strconv.ParseInt(rawViewer, 10, 64)
		if err != nil || id <= 0 {
			writeQualityError(w, http.StatusBadRequest, "bad_viewer", "viewer must be a positive viewer id")
			return
		}
		viewerID = id
	}
	device, ok := resolver.ParseSummaryDevice(q.Get("device"))
	if !ok {
		writeQualityError(w, http.StatusBadRequest, "bad_device", "device must be <field>:<id>")
		return
	}
	if device.Field == "client" && a.db != nil {
		if seen, err := a.seenPlaybackDevices(r.Context()); err == nil {
			for _, d := range seen {
				if d.ClientIdentifier == device.Value {
					id := clientreg.Identity{Product: d.App, Platform: d.Platform, Device: d.Device, DeviceName: d.Name}
					device.ClientGroup = clientreg.Class(id)
					device.ClientType = clientreg.DeviceType(id)
					device.ClientApp = clientreg.AppFor("", id)
					device.ClientCaps = core.ClientCaps{
						ClientID:   d.ClientIdentifier,
						Product:    d.App,
						Platform:   d.Platform,
						Device:     d.Device,
						DeviceName: d.Name,
					}
					break
				}
			}
		}
	}
	var sumLibs []resolver.SummaryLibrary
	if a.libraries != nil {
		libs, err := a.libraries.List(r.Context())
		if err != nil {
			writeQualityError(w, http.StatusInternalServerError, "libraries_failed", "could not list libraries")
			return
		}
		sumLibs = make([]resolver.SummaryLibrary, 0, len(libs))
		for _, lib := range libs {
			pluginName := ""
			chosenByPlugin := false
			if lib.Source == core.LibraryPlugin {
				pluginName = a.pluginDisplayName(r.Context(), lib.PluginID)
				chosenByPlugin = true
			}
			sumLibs = append(sumLibs, resolver.SummaryLibrary{
				ID:             int64(lib.ID),
				Name:           lib.Name,
				MediaType:      lib.MediaType,
				Plugin:         pluginName,
				ChosenByPlugin: chosenByPlugin,
			})
		}
	}
	cfg := a.cfg.Read()
	sum := resolver.SummarizeQuality(cfg.Quality, sumLibs, viewerID, device)
	a.addSummaryDeviceCaps(r.Context(), cfg, &sum, sumLibs, viewerID, q.Get("device"))
	writeQualityJSON(w, http.StatusOK, sum)
}

// addSummaryDeviceCaps fills each cell's device ceiling when the summary is
// filtered to one device or app, evaluated exactly as Check one play does
// (including a rule lifting an Android phone's 1080p ceiling), so a cell
// never says 4K for a browser that tops out at 1080p. A kind of device
// ("Phones & tablets") covers several and gets none.
func (a *App) addSummaryDeviceCaps(ctx context.Context, cfg config.Config, sum *resolver.QualitySummary, libs []resolver.SummaryLibrary, viewerID int64, rawDevice string) {
	field, _, _ := strings.Cut(strings.TrimSpace(rawDevice), ":")
	if field != "type" && field != "app" && field != "client" {
		return
	}
	client, exactID, ok := a.qualityClientByID(ctx, strings.TrimSpace(rawDevice))
	if !ok {
		return
	}
	var u *core.User
	if viewerID > 0 && a.users != nil {
		if loaded, err := a.users.ActiveUser(ctx, viewerID); err == nil {
			u = loaded
		}
	}
	var where [2]core.ClientCaps // at home, away
	for i, remote := range []bool{false, true} {
		declared, _, err := qualityClientCaps(ctx, cfg, client, exactID, remote, u)
		if err != nil {
			return
		}
		where[i] = declared
	}
	for ri := range sum.Rows {
		row := &sum.Rows[ri]
		var lib resolver.SummaryLibrary
		for _, l := range libs {
			if l.ID == row.LibraryID {
				lib = l
				break
			}
		}
		kindOf := func(kind string) string {
			if kind != "" {
				return kind
			}
			if lib.MediaType == core.TypeShow {
				return config.KindEpisode
			}
			return config.KindMovie
		}
		libID := core.LibraryID(row.LibraryID)
		plugin := ""
		if lib.ChosenByPlugin {
			plugin = lib.Plugin
		}
		for ci := range row.Cells {
			cells := &row.Cells[ci]
			for i, cell := range []*resolver.QualitySummaryCell{&cells.Home, &cells.Away} {
				ans := resolver.ExplainQuality(resolver.QualityQuestion{
					Quality:        cfg.Quality,
					Caps:           where[i],
					User:           u,
					Kind:           kindOf(cells.Kind),
					InLibrary:      func(id core.LibraryID) bool { return id == libID },
					ChosenByPlugin: plugin,
				})
				height := where[i].MaxHeight
				for _, l := range ans.Layers {
					if l.Kind == resolver.LayerAndroidLift && l.MaxHeight > 0 {
						height = l.MaxHeight
					}
				}
				cell.DeviceMaxHeight = height
				cell.DeviceMaxBitrateKbps = where[i].MaxBitrateKbps
			}
		}
	}
}

func (a *App) handleQualityExplain(w http.ResponseWriter, r *http.Request) {
	var in qualityExplainIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeQualityError(w, http.StatusBadRequest, "bad_json", "the request body must be a JSON object")
		return
	}
	if in.Kind != "" && in.Kind != config.KindMovie && in.Kind != config.KindEpisode {
		writeQualityError(w, http.StatusBadRequest, "bad_kind", "kind must be movie or episode")
		return
	}
	var client qualityClient
	var exactClientID string
	switch {
	case in.ClientID == "any" || (in.ClientID == "" && in.Client == nil):
		// Generic client with no assumed app.
	case in.ClientID != "":
		found := false
		client, exactClientID, found = a.qualityClientByID(r.Context(), in.ClientID)
		if !found {
			writeQualityError(w, http.StatusBadRequest, "unknown_client", "no such device or app")
			return
		}
	case in.Client != nil && strings.TrimSpace(in.Client.Product) != "":
		client = qualityClient{Product: strings.TrimSpace(in.Client.Product), Platform: strings.TrimSpace(in.Client.Platform), Device: strings.TrimSpace(in.Client.Device), Protocol: in.Client.Protocol}
	default:
		writeQualityError(w, http.StatusBadRequest, "missing_client", "choose a device or app")
		return
	}
	if len(client.Product) > 128 || len(client.Platform) > 128 || len(client.Device) > 128 {
		writeQualityError(w, http.StatusBadRequest, "bad_client", "app fields are too long")
		return
	}
	var u *core.User
	if in.UserID > 0 {
		loaded, err := a.users.ActiveUser(r.Context(), in.UserID)
		if err != nil || loaded == nil {
			writeQualityError(w, http.StatusNotFound, "unknown_viewer", "no such viewer")
			return
		}
		u = loaded
	}
	var title *core.Title
	var inLib resolver.Membership
	var chosenByPlugin string
	kind := in.Kind
	if in.TitleKey != "" {
		key, err := core.ParseRatingKey(in.TitleKey)
		if err != nil {
			writeQualityError(w, http.StatusBadRequest, "bad_title", "invalid title")
			return
		}
		loaded, err := a.catalog.CachedGet(r.Context(), key)
		if err != nil || loaded == nil || (loaded.Type != core.TypeMovie && loaded.Type != core.TypeEpisode) {
			writeQualityError(w, http.StatusNotFound, "unknown_title", "no such movie or episode")
			return
		}
		title = loaded
		if a.libProfiles != nil {
			inLib = func(id core.LibraryID) bool {
				return a.libProfiles.InLibrary(r.Context(), title, id)
			}
		}
	} else if in.LibraryID > 0 {
		if a.libraries == nil {
			writeQualityError(w, http.StatusNotFound, "unknown_library", "no such library")
			return
		}
		lib, err := a.libraries.Get(r.Context(), core.LibraryID(in.LibraryID))
		if err != nil {
			writeQualityError(w, http.StatusNotFound, "unknown_library", "no such library")
			return
		}
		inLib = func(id core.LibraryID) bool { return id == lib.ID }
		if lib.Source == core.LibraryPlugin {
			chosenByPlugin = a.pluginDisplayName(r.Context(), lib.PluginID)
		}
		if kind == "" {
			switch lib.MediaType {
			case core.TypeMovie:
				kind = config.KindMovie
			case core.TypeShow:
				kind = config.KindEpisode
			}
		}
	}
	cfg := a.cfg.Read()
	// The Plex request path: registry, owner App compatibility edits and request
	// headers (remote bitrate caps now live in quality rules).
	declared, builtin, err := qualityClientCaps(r.Context(), cfg, client, exactClientID, in.Remote, u)
	if err != nil {
		writeQualityError(w, http.StatusBadRequest, "bad_client", "cannot evaluate app")
		return
	}

	q := resolver.QualityQuestion{
		Quality:        cfg.Quality,
		Caps:           declared,
		Builtin:        &builtin,
		User:           u,
		Title:          title,
		Kind:           kind,
		InLibrary:      inLib,
		ChosenByPlugin: chosenByPlugin,
	}
	if a.safety != nil && u != nil {
		policy := a.safety.ForUser(u)
		q.RemoteDisabled = policy.RemoteDisabled
	}
	writeQualityJSON(w, http.StatusOK, resolver.ExplainQuality(q))
}

// qualityClientByID resolves a Check one play / summary device id: a seen
// device id (bare or "client:<id>"), a built-in client id, or a
// "type:<name>" / "app:<id>" from /quality/devices. exact is the seen
// device id when it is one.
func (a *App) qualityClientByID(ctx context.Context, id string) (client qualityClient, exact string, found bool) {
	if a.db != nil {
		if seen, err := a.seenPlaybackDevices(ctx); err == nil {
			want := strings.TrimPrefix(id, "client:")
			for _, d := range seen {
				if d.ClientIdentifier == want {
					return qualityClient{ID: d.ClientIdentifier, Name: d.Name, Product: d.App, Platform: d.Platform, Device: d.Device}, d.ClientIdentifier, true
				}
			}
		}
	}
	for _, c := range qualityClients() {
		if c.ID == id {
			return c, "", true
		}
	}
	dev, ok := resolver.ParseSummaryDevice(id)
	if !ok {
		return qualityClient{}, "", false
	}
	switch dev.Field {
	case "type":
		for _, d := range clientreg.Devices() {
			if d.TypeID() == dev.Value {
				return qualityClient{ID: id, Name: d.Name, Product: d.Identity.Product, Platform: d.Identity.Platform, Device: d.Identity.Device, Model: d.Identity.Model}, "", true
			}
		}
	case "app":
		for _, appItem := range clientreg.Apps() {
			if clientreg.AppID(appItem) == dev.Value {
				prod := firstNonEmptyStr(appItem.Identity.Product, appItem.Product, appItem.Name)
				return qualityClient{ID: id, Name: appItem.Name, Product: prod, Platform: appItem.Identity.Platform, Device: appItem.Identity.Device, Protocol: core.Protocol(appItem.Protocol)}, "", true
			}
		}
	}
	return qualityClient{}, "", false
}

// qualityClientCaps is the client as playback declares it (registry, the
// owner's App compatibility edits, request headers) and as built in.
func qualityClientCaps(ctx context.Context, cfg config.Config, client qualityClient, exactClientID string, remote bool, u *core.User) (declared, builtin core.ClientCaps, err error) {
	devName := client.Name
	if devName == "" {
		devName = client.Device
	}
	pr := &plex.Request{ClientID: exactClientID, Product: client.Product, Platform: client.Platform, Device: client.Device, DeviceName: devName, Model: client.Model, Remote: remote, User: u}
	declared = pr.ClientCapsWith(0, cfg.Clients.Overrides)
	builtin = pr.ClientCapsWith(0, nil)
	if frontendHostPolicy(client.Protocol).ProfileQualityEvaluation {
		info := caps.DeviceInfo{Protocol: client.Protocol, ClientID: exactClientID, Product: client.Product, Platform: client.Platform, Device: client.Device, Remote: remote}
		neutral, err := (caps.Builder{}).Build(ctx, info, caps.DeviceProfile{}, nil, "")
		if err != nil {
			return core.ClientCaps{}, core.ClientCaps{}, err
		}
		builtin = playbackFloor(neutral, nil)
		declared = playbackFloor(neutral, cfg.Clients.Overrides)
	} else if client.Product == "" {
		declared = core.ClientCaps{ClientID: exactClientID, Remote: remote, HDR10: true, DolbyVision: true}
		builtin = declared
	}
	return declared, builtin, nil
}

func writeQualityJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeQualityError(w http.ResponseWriter, code int, kind, message string) {
	writeQualityJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": message}})
}
