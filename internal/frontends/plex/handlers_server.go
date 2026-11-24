package plex

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

const (
	transcoderVideoBitrates    = "64,96,208,320,720,1500,2000,3000,4000,8000,10000,12000,20000"
	transcoderVideoQualities   = "0,1,2,3,4,5,6,7,8,9,10,11,12"
	transcoderVideoResolutions = "128,128,160,240,320,480,768,720,720,1080,1080,1080,1080"
)

// APIVersion is the PMS API version Ether implements (X-Plex-Pms-Api-Version).
const APIVersion = "1.2.3"

// serverInfoAttrs are the ServerConfiguration attributes of / (full) and
// /media/providers (providers=true, which omits a few, as PMS does).
// allowSync is the viewer's own download permission: Plex Web's
// showSyncDownload gate reads server.allowSync for library providers.
func (s *Server) serverInfoAttrs(ctx context.Context, providers bool, viewer *frontend.User) []model.Attr {
	id := s.identityCtx(ctx)
	c := s.cfg()
	owner, claimed := s.accountCtx(ctx)
	signIn, mapping := "none", "unknown"
	if claimed {
		signIn, mapping = "ok", "mapped"
	}
	// The transcoder and playback come from the shared session runtime
	// since Prism; the raw deps are empty in production wiring, which told
	// Plex Web the server cannot transcode (#432).
	tc, pb := s.transcoder(), s.playback()
	active := 0
	if tc != nil && pb != nil {
		for _, ps := range pb.Sessions() {
			if len(s.transcodes.forClient(ps.UserID, ps.ClientID, ps.Key)) > 0 {
				active++
			}
		}
	}
	canTranscode := tc != nil
	attrs := []model.Attr{
		model.A("allowCameraUpload", false),
		model.A("allowChannelAccess", false),
		model.A("allowMediaDeletion", false),
		model.A("allowSharing", true),
		model.A("allowSync", s.downloadsAllowed(viewer)),
		model.A("allowTuners", s.liveTVEnabled()),
		model.A("apiVersion", APIVersion),
		model.A("backgroundProcessing", false),
		model.A("certificate", c.Plex.CertFile != ""),
		model.A("companionProxy", true),
		model.A("countryCode", "usa"),
		model.A("diagnostics", ""),
		model.A("eventStream", true),
		model.A("friendlyName", id.FriendlyName),
	}
	if !providers {
		attrs = append(attrs, model.A("hubSearch", true), model.A("itemClusters", false))
	}
	if claimed {
		attrs = append(attrs, model.A("offlineTranscode", 1))
	}
	attrs = append(attrs,
		model.A("livetv", s.liveTVAPIVersion()),
		model.A("machineIdentifier", id.MachineIdentifier),
	)
	if !providers {
		attrs = append(attrs, model.A("mediaProviders", true), model.A("multiuser", true))
	}
	attrs = append(attrs,
		model.A("musicAnalysis", 0),
		model.A("myPlex", claimed),
		model.A("myPlexMappingState", mapping),
		model.A("myPlexSigninState", signIn),
		model.A("myPlexSubscription", id.MyPlexSubscription),
		model.A("myPlexUsername", owner),
		model.A("ownerFeatures", strings.Join(id.OwnerFeatures, ",")),
		model.A("platform", id.Platform),
		model.A("platformVersion", id.PlatformVersion),
		model.A("pluginHost", false),
		model.A("pushNotifications", false),
		model.A("readOnlyLibraries", false),
		model.A("streamingBrainABRVersion", 3),
		model.A("streamingBrainVersion", 2),
		model.A("sync", s.downloadsEnabled()),
		model.A("transcoderActiveVideoSessions", active),
		model.A("transcoderAudio", canTranscode),
		model.A("transcoderLyrics", false),
	)
	if !providers {
		attrs = append(attrs, model.A("transcoderPhoto", true))
	}
	attrs = append(attrs,
		model.A("transcoderSubtitles", canTranscode),
		model.A("transcoderVideo", canTranscode),
		model.A("transcoderVideoBitrates", transcoderVideoBitrates),
		model.A("transcoderVideoQualities", transcoderVideoQualities),
		model.A("transcoderVideoResolutions", transcoderVideoResolutions),
		model.A("updatedAt", s.now().Unix()),
		model.A("updater", false),
		model.A("version", id.Version),
		model.A("voiceSearch", false),
	)
	return attrs
}

var rootDirectories = []string{
	"actions", "activities", "butler", "channels", "clients", "devices", "diagnostics", "hubs", "library",
	"livetv", "media", "neighborhood", "playQueues", "playlists", "resources", "search", "server",
	"servers", "statistics", "system", "transcode", "updater", "user",
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{Extra: s.serverInfoAttrs(r.Context(), false, FromContext(r.Context()).User)}
	for _, d := range rootDirectories {
		mc.Directory = append(mc.Directory, model.Directory{Count: 1, Key: d, Title: d})
	}
	mc.Size = len(mc.Directory)
	applyLegacyServerVersion(r, mc)
	s.write(w, r, mc)
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	id := s.identityCtx(r.Context())
	claimed := s.cfg().Plex.Claimed
	if o, ok := r.Context().Value(identityOverrideKey{}).(frontend.ServerIdentity); ok && o.Account != nil {
		claimed = o.Account.Claimed
	}
	s.write(w, r, &model.MediaContainer{Extra: []model.Attr{
		model.A("apiVersion", APIVersion),
		model.A("claimed", claimed),
		model.A("machineIdentifier", id.MachineIdentifier),
		model.A("version", id.Version),
	}})
}

// sectionPivots are the top-level navigation pivots of a section.
func sectionPivots(sec Section) []model.Pivot {
	id := strconv.Itoa(sec.ID)
	typeNum := strconv.Itoa(typeNumber(sec.Type))
	playlistType := "video"
	switch string(sec.Type) {
	case plexBaseArtist:
		playlistType = "audio"
	case plexBasePhoto:
		playlistType = "photo"
	}
	return []model.Pivot{
		{ID: "recommended", Key: "/hubs/sections/" + id, Type: "hub", Title: "Recommended", Context: "content.discover", Symbol: "star"},
		{ID: "library", Key: "/library/sections/" + id + "/all?type=" + typeNum, Type: "list", Title: "Library", Context: "content.library", Symbol: "library"},
		{ID: "collections", Key: "/library/sections/" + id + "/collections", Type: "list", Title: "Collections", Context: "content.collections", Symbol: "stack"},
		{ID: "playlists", Key: "/playlists?type=15&sectionID=" + id + "&playlistType=" + playlistType, Type: "list", Title: "Playlists", Context: "content.playlists", Symbol: "playlist"},
		{ID: "categories", Key: "/library/sections/" + id + "/categories", Type: "list", Title: "Categories", Context: "content.categories", Symbol: "stack"},
	}
}

func providerTypes(secs []Section) string {
	var hasVideo, hasAudio, hasPhoto bool
	for _, sec := range secs {
		switch string(sec.Type) {
		case plexBaseArtist:
			hasAudio = true
		case plexBasePhoto:
			hasPhoto = true
		default:
			hasVideo = true
		}
	}
	var out []string
	if hasVideo || (!hasAudio && !hasPhoto) {
		out = append(out, "video")
	}
	if hasAudio {
		out = append(out, "audio")
	}
	if hasPhoto {
		out = append(out, "photo")
	}
	return strings.Join(out, ",")
}

func agentFor(t frontend.MediaType) (agent, scanner string) {
	switch string(t) {
	case plexBaseShow:
		return "tv.plex.agents.series", "Plex TV Series"
	case plexBaseArtist:
		return "tv.plex.agents.music", "Plex Music"
	case plexBasePhoto:
		return "tv.plex.agents.photo", "Plex Photo"
	default:
		return "tv.plex.agents.movie", "Plex Movie"
	}
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	u := FromContext(r.Context()).User
	now := s.now().Unix()
	includePrefs := r.URL.Query().Get("includePreferences") == "1"
	userSecs := s.userSections(u)
	dirs := []model.ProviderDirectory{{HubKey: "/hubs", Title: "Home"}} // discovery only: no browse key
	for _, sec := range userSecs {
		agent, scanner := agentFor(sec.Type)
		id := strconv.Itoa(sec.ID)
		dir := model.ProviderDirectory{
			Agent:      agent,
			Language:   sec.Language,
			Refreshing: model.Ptr(false),
			Scanner:    scanner,
			UUID:       sec.UUID,
			ID:         id,
			Key:        "/library/sections/" + id,
			HubKey:     "/hubs/sections/" + id,
			Type:       string(sec.Type),
			Title:      sec.Title,
			UpdatedAt:  now,
			ScannedAt:  now,
			Pivot:      sectionPivots(sec),
		}
		if includePrefs {
			// PMS repeats the section's /prefs settings here; Roku crashes
			// opening a library whose entry has none (#652).
			dir.Preferences = &model.TrackPreferences{Setting: s.sectionPrefs(sec)}
		}
		dirs = append(dirs, dir)
	}
	dirs = append(dirs, model.ProviderDirectory{
		ID: "playlists", Key: "/playlists", Type: "playlist", Title: "Playlists",
		Pivot: []model.Pivot{{ID: "playlists.video", Key: "/playlists?playlistType=video", Type: "list", Title: "Video", Context: "content.playlists.video", Symbol: "playlist"}},
	})
	if legacyRoku(FromContext(r.Context())) {
		for i := range dirs {
			dirs[i].Pivot = nil
			if dirs[i].ID != "playlists" {
				dirs[i].ID = ""
			}
		}
	}
	provider := model.MediaProvider{
		Identifier: model.LibraryIdentifier,
		Title:      "Library",
		Types:      providerTypes(userSecs),
		Protocols:  "stream,download",
		Feature: []model.Feature{
			{Key: "/library/sections", Type: "content", Directory: dirs},
			{Key: "/hubs/search", Type: "search"},
			{Key: "/library/matches", Type: "match"},
			{Key: "/library/metadata", Type: "metadata"},
			{Key: "/:/rate", Type: "rate"},
			{Key: "/photo/:/transcode", Type: "imagetranscoder"},
			{Key: "/hubs/promoted", Type: "promoted"},
			{Key: "/hubs/continueWatching", Type: "continuewatching"},
			{Key: "/actions", Type: "actions", Action: []model.Action{{ID: "removeFromContinueWatching", Key: "/actions/removeFromContinueWatching"}}},
			{Flavor: "universal", Key: "/playlists", Type: "playlist"},
			{Flavor: "universal", Key: "/playQueues", Type: "playqueue"},
			{Key: "/library/collections", Type: "collection"},
			{ScrobbleKey: "/:/scrobble", UnscrobbleKey: "/:/unscrobble", Key: "/:/timeline", Type: "timeline"},
			{Type: "queryParser"},
			{Key: "/library/search", Type: "universalsearch"},
		},
	}
	if s.downloadsAllowed(u) {
		provider.Feature = append(provider.Feature, downloadSubscribeFeature)
	}
	mc := &model.MediaContainer{Size: 1, Extra: s.serverInfoAttrs(r.Context(), true, u), MediaProvider: []model.MediaProvider{provider}}
	if p := s.liveTVProvider(r.Context(), FromContext(r.Context()).User); p != nil {
		mc.MediaProvider = append(mc.MediaProvider, *p)
		mc.Size++
	}
	applyLegacyServerVersion(r, mc)
	s.write(w, r, mc)
}

// Older Roku clients cannot consume provider pivots. PMS compares only the
// numeric components supplied by the client, so 6.4 satisfies 6.4.2.
func legacyRoku(pr *Request) bool {
	if !strings.EqualFold(pr.Platform, "Roku") {
		return false
	}
	// Only major.minor.patch matters. Current apps send a fourth part and a
	// build suffix ("9.21.12.12191-2cb8e41a8-Plex"), and a part may carry a
	// tag ("9.22.1-beta"), so each part is read up to its first non-digit.
	parts := strings.Split(pr.Version, ".")
	if len(parts) < 2 {
		return true
	}
	if len(parts) > 3 {
		parts = parts[:3]
	}
	minimum := []int{6, 4, 2}
	for i, part := range parts {
		end := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' })
		if end < 0 {
			end = len(part)
		}
		n, err := strconv.Atoi(part[:end])
		if err != nil {
			return true
		}
		if n != minimum[i] {
			return n < minimum[i]
		}
	}
	return false
}

func applyLegacyServerVersion(r *http.Request, mc *model.MediaContainer) {
	if FromContext(r.Context()).Product != "Plex Home Theater" {
		return
	}
	for i := range mc.Extra {
		if mc.Extra[i].Name != "version" {
			continue
		}
		version, ok := mc.Extra[i].Value.(string)
		if !ok {
			continue
		}
		number, build, hasBuild := strings.Cut(version, "-")
		if strings.Count(number, ".") == 3 {
			number += ".0"
			if hasBuild {
				number += "-" + build
			}
			mc.Extra[i].Value = number
		}
	}
}

func (s *Server) handleMyPlexAccount(w http.ResponseWriter, r *http.Request) {
	c := s.cfg()
	owner, claimed := s.accountCtx(r.Context())
	acct := model.MyPlex{
		Username:          owner,
		MappingState:      "unknown",
		SignInState:       "none",
		SubscriptionState: "Unknown",
	}
	if claimed {
		acct.MappingState = "mapped"
		acct.SignInState = "ok"
	}
	id := s.identityCtx(r.Context())
	acct.SubscriptionActive = id.MyPlexSubscription
	if id.MyPlexSubscription {
		acct.SubscriptionState = "Active"
		acct.SubscriptionFeatures = strings.Join(id.OwnerFeatures, ",")
	}
	if c.Plex.PublicHost != "" {
		acct.PublicAddress = c.Plex.PublicHost
		acct.PublicPort = c.Plex.PublicPort
		if acct.PublicPort == 0 {
			acct.PublicPort = c.Server.Port
		}
	}
	if host, _, err := net.SplitHostPort(r.Host); err == nil && host != "" {
		acct.PrivateAddress = host
		acct.PrivatePort = c.Server.Port
	}
	s.write(w, r, &acct)
}

func (s *Server) handleRefreshReachability(w http.ResponseWriter, r *http.Request) {
	c := s.cfg()
	reachable := c.Plex.RemoteAccess && (c.Plex.Claimed || c.Plex.Token != "")
	s.publishNotification(model.NotificationContainer{
		Type: "reachability",
		Size: 1,
		ReachabilityNotification: []model.ReachabilityNotification{{
			Reachability: reachable,
		}},
	}, 0)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	id := s.identityCtx(r.Context())
	host, portS, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	port, _ := strconv.Atoi(portS)
	if port == 0 {
		port = s.cfg().Server.Port
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Server: []model.Server{{
		Name: id.FriendlyName, Host: host, Address: host, Port: port,
		MachineIdentifier: id.MachineIdentifier, Version: id.Version,
	}}})
}

func (s *Server) handlePrefs(w http.ResponseWriter, r *http.Request) {
	c := s.cfg()
	id := s.identityCtx(r.Context())
	secure := map[string]int{"required": 0, "preferred": 1, "disabled": 2}[c.Plex.SecureConnections]
	if c.Plex.SecureConnections == "" {
		secure = 1
	}
	port := c.Plex.PublicPort
	if port == 0 {
		port = c.Server.Port
	}
	// Values are typed (string, int, bool) as in PMS JSON; XML renders bools
	// as 1/0.
	settings := []model.Setting{
		{ID: "FriendlyName", Label: "Friendly name", Summary: "This name will be used to identify this media server to other computers on your network.", Type: "text", Default: "", Value: id.FriendlyName, Group: "general"},
		// PMS lists these after its general settings, before MachineIdentifier.
		{ID: "OnDeckWindow", Label: "Weeks to consider for Continue Watching", Summary: "Movies not watched in this many weeks leave Continue Watching. Unfinished series remain eligible regardless of age.", Type: "int", Default: 16, Value: 16, Group: "library", Advanced: true},
		{ID: "OnDeckLimit", Label: "Maximum number of Continue Watching items which will appear.", Summary: "Limits the number of shows which will appear Continue Watching. Setting it too high can affect performance.", Type: "int", Default: plexOnDeckLimit, Value: plexOnDeckLimit, Group: "library", Advanced: true},
		{ID: "OnDeckIncludePremieres", Label: "Include season premieres in Continue Watching", Summary: "Unfinished series, including season premieres, remain eligible regardless of age.", Type: "bool", Default: true, Value: true, Group: "library", Advanced: true},
		{ID: "MachineIdentifier", Label: "", Summary: "", Type: "text", Default: "", Value: id.MachineIdentifier, Hidden: true, Group: ""},
		{ID: "PublishServerOnPlexOnlineKey", Label: "Publish server on Plex Online", Summary: "", Type: "bool", Default: false, Value: c.Plex.RemoteAccess, Group: "network"},
		{ID: "ManualPortMappingMode", Label: "Manually specify public port", Summary: "", Type: "bool", Default: false, Value: c.Plex.PublicPort != 0, Group: "network"},
		{ID: "ManualPortMappingPort", Label: "", Summary: "", Type: "int", Default: 32400, Value: port, Hidden: true, Group: ""},
		{ID: "secureConnections", Label: "Secure connections", Summary: "", Type: "int", Default: 1, Value: secure, Advanced: true, Group: "network", EnumValues: "1:Preferred|0:Required"},
		{ID: "customConnections", Label: "Custom server access URLs", Summary: "", Type: "text", Default: "", Value: strings.Join(c.Plex.CustomURLs, ","), Group: "network", Advanced: true},
		{ID: "WanPerStreamMaxUploadRate", Label: "Limit remote stream bitrate", Summary: "", Type: "int", Default: 0, Value: c.Transcode.RemoteMaxBitrateKbps, Group: "network"},
		{ID: "TranscoderTempDirectory", Label: "Transcoder temporary directory", Summary: "", Type: "text", Default: "", Value: c.Transcode.TempDir, Group: "transcoder", Advanced: true},
		{ID: "HardwareAcceleratedCodecs", Label: "Use hardware acceleration when available", Summary: "", Type: "bool", Default: true, Value: c.Transcode.HWAccel != "none", Group: "transcoder"},
		{ID: "TranscodeCountLimit", Label: "Maximum simultaneous video transcode", Summary: "", Type: "int", Default: 0, Value: c.Transcode.MaxTranscodes, Group: "transcoder", Advanced: true},
		{ID: "sendCrashReports", Label: "Send crash reports to Plex", Summary: "", Type: "bool", Default: true, Value: false, Group: "general"},
		{ID: "logDebug", Label: "Enable Plex Media Server debug logging", Summary: "", Type: "bool", Default: true, Value: c.Server.LogLevel == "debug", Group: "general", Advanced: true},
	}
	if id := r.URL.Query().Get("id"); id != "" {
		var only []model.Setting
		for _, st := range settings {
			if st.ID == id {
				only = append(only, st)
			}
		}
		settings = only
	}
	s.write(w, r, &model.MediaContainer{Size: len(settings), Setting: settings})
}

// handlePrefsSet accepts and ignores PMS settings writes; Ether is
// configured through its own admin UI.
func (s *Server) handlePrefsSet(w http.ResponseWriter, r *http.Request) {
	s.log.Info("ignoring PMS prefs write; use the Ether admin UI", "query", redactQuery(r.URL.RawQuery))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents := []model.Agent{
		{Identifier: "tv.plex.agents.movie", Primary: true, Name: "Plex Movie", MediaType: []model.MediaType{{MediaType: 1, Name: "movie"}}},
		{Identifier: "tv.plex.agents.series", Primary: true, Name: "Plex Series", MediaType: []model.MediaType{{MediaType: 2, Name: "show"}}},
		{Identifier: "com.plexapp.agents.none", Name: "Personal Media", MediaType: []model.MediaType{{MediaType: 1, Name: "movie"}, {MediaType: 2, Name: "show"}}},
	}
	s.write(w, r, &model.MediaContainer{Size: len(agents), Agent: agents})
}

func (s *Server) modelAccount(ctx context.Context, u frontend.User) model.Account {
	id := plexAccountID(u)
	a := model.Account{
		ID:              id,
		Key:             "/accounts/" + strconv.FormatInt(id, 10),
		Name:            plexAccountName(u),
		AutoSelectAudio: true,
		Thumb:           u.Thumb,
	}
	if len(u.AudioLanguages) > 0 {
		a.DefaultAudioLanguage = u.AudioLanguages[0]
	}
	if len(u.SubtitleLanguages) > 0 {
		a.DefaultSubtitleLang = u.SubtitleLanguages[0]
	}
	if tp := s.svc().trackPrefs; tp != nil {
		if rules, err := tp.GetRules(ctx, u.ID); err == nil {
			// Project the effective (layered) preferences: account,
			// household (user 0) and viewer rules all count, so a viewer
			// who inherits Always-on/English reads the same here as one
			// who saved it themselves (#617). With nothing configured at
			// any layer GetRules returns the defaults (mode off, no
			// languages), which leaves this document unchanged.
			// Plex account subtitleMode: 0 manual, 1 with foreign audio, 2 always.
			a.SubtitleMode = plexSubtitleMode(rules.SubtitleMode)
			if len(rules.SubtitleLanguages) > 0 {
				a.DefaultSubtitleLang = rules.SubtitleLanguages[0]
			}
			if len(rules.AudioLanguages) > 0 {
				a.DefaultAudioLanguage = rules.AudioLanguages[0]
			}
		}
	}
	return a
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	var users []frontend.User
	if s.getHost().Identity != nil {
		if us, err := s.listUsers(r.Context()); err == nil {
			users = us
		} else {
			s.log.Warn("list users", "err", err)
		}
	}
	mc := &model.MediaContainer{Identifier: "com.plexapp.system.accounts"}
	for _, u := range users {
		mc.Account = append(mc.Account, s.modelAccount(r.Context(), u))
	}
	mc.Size = len(mc.Account)
	s.write(w, r, mc)
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := parseWhole(chi.URLParam(r, "id"))
	if !ok || id <= 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	dir := s.newPlexAccountDir(r.Context(), FromContext(r.Context()).User, true)
	entry, found := dir.resolve(r.Context(), id)
	if !found {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.write(w, r, &model.MediaContainer{
		Size:       1,
		Identifier: "com.plexapp.system.accounts",
		Account:    []model.Account{s.modelAccount(r.Context(), entry.user)},
	})
}

// plexSubtitleMode matches Plex's account attribute: 0 is manual, 1 is shown
// with foreign audio, 2 is always. Forced-only has no Plex value; 1 tells the
// client to enable subtitles while Ether's selector still picks a forced track.
// Off stays 0, the same attribute every unchanged account already carries.
func plexSubtitleMode(mode string) int {
	switch mode {
	case "always":
		return 2
	case "foreign-only", "forced-only":
		return 1
	default:
		return 0
	}
}

func (s *Server) handleSecurityToken(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	id := s.getHost().Identity
	if id == nil || pr.User == nil || pr.User.Delegated {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	token, err := id.IssueDelegation(r.Context(), pr.User.ID)
	if errors.Is(err, frontend.ErrUnauthorized) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.write(w, r, &model.MediaContainer{Extra: []model.Attr{model.A("token", token)}})
}

func (s *Server) handleUpdaterStatus(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &model.MediaContainer{Extra: []model.Attr{
		model.A("autoUpdateVersion", 1),
		model.A("canInstall", false),
		model.A("checkedAt", 0),
		model.A("downloadURL", ""),
		model.A("status", 0),
	}})
}

// handleUnknown answers endpoints Ether doesn't implement the way PMS
// answers paths it does not know: 404 with its HTML error page. An empty
// 200 container here would make a mistyped or unsupported request look
// like an empty result; routes Ether knowingly stubs answer with
// writeEmpty explicitly in routes().
func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	s.log.Debug("unhandled endpoint", "method", r.Method, "path", r.URL.Path, "query", redactQuery(r.URL.RawQuery))
	writeHTTPError(w, http.StatusNotFound)
}

func (s *Server) handleOK(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// redactQuery strips tokens from a logged query string.
func redactQuery(q string) string {
	parts := strings.Split(q, "&")
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		// Authentication URL-decodes parameter names. Apply the same decoding
		// before classifying secrets, while preserving unrelated diagnostics.
		name, err := url.QueryUnescape(k)
		if err == nil && (strings.EqualFold(name, "X-Plex-Token") || strings.EqualFold(name, streamGrantParam) || strings.EqualFold(name, readCapParam) || strings.EqualFold(name, deliveryGrantParam)) {
			parts[i] = k + "=REDACTED"
		}
	}
	return strings.Join(parts, "&")
}
