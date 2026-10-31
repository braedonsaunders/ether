package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// errViewerScope means the request named a viewer other than the grant holder.
var errViewerScope = errors.New("viewer mismatch")

func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}
func (i *instance) identityContext(r *http.Request) context.Context {
	if i.host.ClientIP != nil {
		if ip, ok := i.host.ClientIP(r); ok {
			return frontend.WithClientIP(r.Context(), ip)
		}
	}
	return r.Context()
}

type verifiedDeviceKey struct{}

func device(r *http.Request) frontend.Device {
	if stored, ok := r.Context().Value(verifiedDeviceKey{}).(frontend.Device); ok {
		return stored
	}
	p, _ := authhdr.MediaBrowser(r.Header.Get("Authorization"))
	if len(p) == 0 {
		p, _ = authhdr.MediaBrowser(r.Header.Get("X-Emby-Authorization"))
	}
	get := func(key, header string) string {
		if value := p[key]; value != "" {
			return value
		}
		if value := r.Header.Get(header); value != "" {
			return value
		}
		if values := route.Query(r.URL.Query(), key); len(values) == 1 {
			return values[0]
		}
		return ""
	}
	client := get("Client", "X-Emby-Client")
	return frontend.Device{ID: get("DeviceId", "X-Emby-Device-Id"), Client: client, Product: client, Version: get("Version", "X-Emby-Client-Version"), DeviceName: get("Device", "X-Emby-Device-Name")}

}
func (i *instance) viewer(r *http.Request) (*frontend.Viewer, string, error) {
	token := authhdr.MediaBrowserToken(r)
	if token == "" {
		return nil, "", errors.New("missing credential")
	}
	v, err := i.host.Identity.Resolve(r.Context(), token)
	if err == nil && v != nil {
		if grants, ok := i.host.Identity.(frontend.GrantDeviceIdentity); ok {
			claim := device(r)
			boundViewer, stored, _, bindingErr := grants.ResolveGrantDevice(r.Context(), token, claim.ID)
			if bindingErr != nil || boundViewer == nil || boundViewer.ID != v.ID || stored.ID == "" || (claim.ID != "" && claim.ID != stored.ID) {
				return nil, "", errors.New("invalid device grant")
			}
			// Device ID is authoritative from the grant; client metadata is
			// descriptive and may change as the application is upgraded.
			if claim.Client != "" {
				stored.Client, stored.Product = claim.Client, claim.Product
			}
			if claim.Version != "" {
				stored.Version = claim.Version
			}
			stored.DeviceName, stored.Platform, stored.Language = claim.DeviceName, claim.Platform, claim.Language
			*r = *r.WithContext(context.WithValue(r.Context(), verifiedDeviceKey{}, stored))
			v = boundViewer
		}
	}
	if err == nil && v != nil {
		scoped := r.PathValue("userId")
		if scoped == "" {
			values := route.Query(r.URL.Query(), "userId")
			if len(values) > 1 {
				return nil, "", errors.New("ambiguous user")
			}
			if len(values) == 1 {
				scoped = values[0]
			}
		}
		if scoped != "" {
			id, e := ids.ParseGUID(ids.User, scoped)
			if e != nil || id != v.ID {
				return nil, "", errViewerScope
			}
		}
	}
	if err == nil && v != nil && i.host.ClientSeen != nil {
		i.host.ClientSeen(device(r).ID)
	}
	return v, token, err
}
func (i *instance) userDTO(r *http.Request, v *frontend.Viewer) map[string]any {
	id, _ := ids.GUID(ids.User, v.ID)
	// Jellyfin's Kotlin SDK requires these keys even where its OpenAPI schema
	// omits `required`. Unsupported features are false; the broker and host
	// remain authoritative for every actual operation.
	policy := map[string]any{
		"IsAdministrator": v.IsOwner && !v.Delegated, "IsHidden": true, "IsDisabled": false,
		"EnableCollectionManagement": false, "EnableSubtitleManagement": false, "EnableLyricManagement": false,
		"EnableUserPreferenceAccess": i.host.Preferences != nil, "EnableRemoteControlOfOtherUsers": false,
		"EnableSharedDeviceControl": false, "EnableRemoteAccess": false, "EnableLiveTvManagement": false, "EnableLiveTvAccess": false,
		"EnableMediaPlayback": i.host.Sessions != nil, "EnablePlaybackRemuxing": i.host.Sessions != nil,
		// The host's configured playback policy overrides these defaults below.
		"EnableAudioPlaybackTranscoding": i.host.Sessions != nil, "EnableVideoPlaybackTranscoding": i.host.Sessions != nil,
		"ForceRemoteSourceTranscoding": false, "EnableContentDeletion": false, "EnableContentDownloading": false,
		"EnableSyncTranscoding": false, "EnableMediaConversion": false, "EnableAllDevices": true, "EnableAllChannels": true,
		"EnabledDevices": []string{}, "EnabledChannels": []string{}, "EnableContentDeletionFromFolders": []string{}, "AllowedTags": []string{}, "BlockedTags": []string{},
		"EnableAllFolders": len(v.AllowedSections) == 0, "InvalidLoginAttemptCount": 0, "LoginAttemptsBeforeLockout": -1,
		"MaxActiveSessions": v.StreamLimit, "EnablePublicSharing": false, "RemoteClientBitrateLimit": 0,
		"AuthenticationProviderId": "Ether", "PasswordResetProviderId": "Ether", "SyncPlayAccess": "None",
	}
	if i.protocol == "jellyfin" {
		policy["PasswordResetProviderId"] = "Ether"
	}
	// Presentation defaults mirror Jellyfin's UserConfiguration constructor.
	configuration := map[string]any{"PlayDefaultAudioTrack": true, "DisplayMissingEpisodes": false, "GroupedFolders": []string{}, "SubtitleMode": "Default", "DisplayCollectionsView": false, "EnableLocalPassword": false, "OrderedViews": []string{}, "LatestItemsExcludes": []string{}, "MyMediaExcludes": []string{}, "HidePlayedInLatest": true, "RememberAudioSelections": true, "RememberSubtitleSelections": true, "EnableNextEpisodeAutoPlay": true}
	if len(v.AudioLanguages) > 0 {
		configuration["AudioLanguagePreference"] = v.AudioLanguages[0]
		configuration["PlayDefaultAudioTrack"] = false
	}
	if len(v.SubtitleLanguages) > 0 {
		configuration["SubtitleLanguagePreference"] = v.SubtitleLanguages[0]
	}
	if i.host.TrackPreferences != nil {
		if prefs, err := i.host.TrackPreferences.Get(r.Context(), v); err == nil {
			if len(prefs.SubtitleLanguages) > 0 {
				configuration["SubtitleLanguagePreference"] = prefs.SubtitleLanguages[0]
			}
			if mode, ok := jellyfinSubtitleMode(prefs.SubtitleMode); ok {
				configuration["SubtitleMode"] = mode
			}
		}
	}
	i.applyOrderedViews(r, v, configuration)
	if i.protocol == "emby" {
		for _, key := range []string{"AllowedTags", "EnableCollectionManagement", "EnableLyricManagement", "ForceRemoteSourceTranscoding", "LoginAttemptsBeforeLockout", "MaxActiveSessions", "PasswordResetProviderId", "SyncPlayAccess"} {
			delete(policy, key)
		}
		policy["SimultaneousStreamLimit"] = v.StreamLimit
		delete(configuration, "DisplayCollectionsView")
		delete(configuration, "GroupedFolders")
	}
	result := map[string]any{"Id": id, "Name": v.Username, "ServerId": i.info.ID, "Policy": policy, "Configuration": configuration}
	if i.protocol == "emby" {
		defer i.embyUserDTO(result)
	}
	if i.protocol == "emby" && !v.CreatedAt.IsZero() {
		result["DateCreated"] = wireTime(v.CreatedAt)
	}
	if i.protocol == "jellyfin" {
		// These keys are always present on Jellyfin 12.4 UserDto. Empty values
		// are facts: Ether has no Chromecast receiver, easy password, auto-login,
		// access schedule, or blocked channel/folder list.
		if _, ok := configuration["SubtitleLanguagePreference"]; !ok {
			configuration["SubtitleLanguagePreference"] = ""
		}
		configuration["CastReceiverId"] = ""
		result["EnableAutoLogin"] = false
		result["HasConfiguredEasyPassword"] = false
		policy["AccessSchedules"] = []any{}
		policy["BlockUnratedItems"] = []any{}
		policy["BlockedChannels"] = []any{}
		policy["BlockedMediaFolders"] = []any{}
	}
	if i.host.ViewerCapabilities == nil {
		return result
	}
	var ip netip.Addr
	if i.host.ClientIP != nil {
		ip, _ = i.host.ClientIP(r)
	}
	caps, err := i.host.ViewerCapabilities(r.Context(), v, device(r), ip)
	if err != nil {
		return result
	}
	if caps.IsOwner != nil {
		policy["IsAdministrator"] = *caps.IsOwner
	}
	if caps.CanPlayRemote != nil {
		policy["EnableRemoteAccess"] = *caps.CanPlayRemote
	}
	if caps.CanPlay != nil {
		policy["EnableMediaPlayback"] = *caps.CanPlay
	}
	if caps.CanUseLiveTV != nil {
		policy["EnableLiveTvAccess"] = *caps.CanUseLiveTV
	}
	if caps.CanTranscode != nil {
		policy["EnableVideoPlaybackTranscoding"] = *caps.CanTranscode
		policy["EnableAudioPlaybackTranscoding"] = *caps.CanTranscode
	}
	if caps.CanTranscodeAudio != nil {
		policy["EnableAudioPlaybackTranscoding"] = *caps.CanTranscodeAudio
	}
	if caps.CanTranscodeVideo != nil {
		policy["EnableVideoPlaybackTranscoding"] = *caps.CanTranscodeVideo
	}
	if caps.CanRemux != nil {
		policy["EnablePlaybackRemuxing"] = *caps.CanRemux
	}
	if caps.MaxActiveSessions != nil {
		if i.protocol == "emby" {
			policy["SimultaneousStreamLimit"] = *caps.MaxActiveSessions
		} else {
			policy["MaxActiveSessions"] = *caps.MaxActiveSessions
		}
	}
	if caps.BlockedTags != nil {
		policy["BlockedTags"] = *caps.BlockedTags
	}
	if caps.RemoteBitrateKbps != nil {
		policy["RemoteClientBitrateLimit"] = *caps.RemoteBitrateKbps * 1000
	}
	if caps.HasLocalPassword != nil {
		result["HasPassword"] = *caps.HasLocalPassword
		result["HasConfiguredPassword"] = *caps.HasLocalPassword
		if i.host.Config != nil {
			if enabled, ok := i.host.Config()["publicViewerPicker"].(bool); ok && enabled && *caps.HasLocalPassword {
				policy["IsHidden"] = false
			}
		}
	}
	if caps.AllowedLibraries != nil {
		folders := make([]string, 0, len(*caps.AllowedLibraries))
		for _, view := range *caps.AllowedLibraries {
			n, e := ids.ParseDecimal(view.ID)
			if e != nil {
				continue
			}
			guid, e := ids.GUID(ids.View, n)
			if e == nil {
				folders = append(folders, guid)
			}
		}
		policy["EnableAllFolders"] = false
		policy["EnabledFolders"] = folders
	}
	return result
}
func (i *instance) sessionDTO(r *http.Request, v *frontend.Viewer) map[string]any {
	d := device(r)
	userID, _ := ids.GUID(ids.User, v.ID)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", i.protocol, v.ID, d.ID)))
	sessionID := hex.EncodeToString(sum[:16])
	peer := ""
	if i.host.ClientIP != nil {
		if ip, ok := i.host.ClientIP(r); ok {
			peer = ip.String()
		}
	}
	now := time.Now().UTC()
	state := map[string]any{"CanSeek": false, "IsPaused": false, "IsMuted": false, "RepeatMode": "RepeatNone"}
	session := map[string]any{"Id": sessionID, "UserId": userID, "UserName": v.Username, "ServerId": i.info.ID, "Client": d.Client, "DeviceId": d.ID, "DeviceName": d.DeviceName, "ApplicationVersion": d.Version, "LastActivityDate": wireTime(now), "RemoteEndPoint": peer, "AdditionalUsers": []any{}, "PlayableMediaTypes": []any{}, "SupportedCommands": []any{}, "SupportsRemoteControl": false, "PlayState": state}
	if i.protocol == "jellyfin" {
		// This sign-in response precedes any playback check-in.
		session["LastPlaybackCheckIn"] = wireTime(time.Time{})
		state["PlaybackOrder"] = "Default"
		session["IsActive"] = true
		session["HasCustomDeviceName"] = false
		session["SupportsMediaControl"] = false
		session["NowPlayingQueue"] = []any{}
		session["Capabilities"] = map[string]any{"PlayableMediaTypes": []any{}, "SupportedCommands": []any{}, "SupportsMediaControl": false, "SupportsPersistentIdentifier": d.ID != ""}
	} else {
		state["PlaybackRate"] = 1
		state["Shuffle"] = false
		state["SleepTimerMode"] = "None"
		state["SubtitleOffset"] = 0
		session["Protocol"] = r.Proto
		session["PlaylistIndex"] = 0
		session["PlaylistLength"] = 0
	}
	if i.host.Sessions != nil {
		caps, err := i.host.Sessions.Capabilities(r.Context(), frontend.SessionAccess{Viewer: v, Device: d, Protocol: i.protocol})
		if err == nil {
			c := sessionCapabilitiesDTO(caps)
			if i.protocol == "jellyfin" {
				session["Capabilities"] = c
			}
			for _, key := range []string{"PlayableMediaTypes", "SupportedCommands", "SupportsRemoteControl", "SupportsMediaControl"} {
				if i.protocol == "emby" && key == "SupportsMediaControl" {
					continue
				}
				if value, ok := c[key]; ok {
					session[key] = value
				}
			}
			if caps.SupportsRemoteControl != nil {
				session["SupportsRemoteControl"] = *caps.SupportsRemoteControl
			}
		}
	}
	if i.protocol == "emby" {
		i.embySessionDTO(r.Context(), session, d.ID)
	}
	return session
}

func sessionCapabilitiesDTO(c frontend.SessionCapabilities) map[string]any {
	mediaTypes, commands := c.PlayableMediaTypes, c.SupportedCommands
	if mediaTypes == nil {
		mediaTypes = []string{}
	}
	if commands == nil {
		commands = []string{}
	}
	m := map[string]any{"PlayableMediaTypes": mediaTypes, "SupportedCommands": commands, "SupportsMediaControl": false, "SupportsPersistentIdentifier": false}
	if c.SupportsMediaControl != nil {
		m["SupportsMediaControl"] = *c.SupportsMediaControl
	}
	if c.SupportsPersistentIdentifier != nil {
		m["SupportsPersistentIdentifier"] = *c.SupportsPersistentIdentifier
	}
	if len(c.DeviceProfile) > 0 {
		m["DeviceProfile"] = c.DeviceProfile
	}
	if c.AppStoreURL != "" {
		m["AppStoreUrl"] = c.AppStoreURL
	}
	if c.IconURL != "" {
		m["IconUrl"] = c.IconURL
	}
	return m
}
func (i *instance) result(w http.ResponseWriter, r *http.Request, token string) {
	v, err := i.host.Identity.Resolve(r.Context(), token)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if warm, ok := i.host.VirtualPaths.(interface {
		Warm(context.Context, *frontend.Viewer, frontend.Device)
	}); ok {
		warm.Warm(context.WithoutCancel(r.Context()), v, device(r))
	}
	jsonReply(w, map[string]any{"AccessToken": token, "ServerId": i.info.ID, "User": i.userDTOWithTimes(r, v, token), "SessionInfo": i.sessionDTO(r, v)})
}

// fastResult answers a sign-in with the token and a small user. Home rows,
// viewer timestamps, session capability lookups and path warming wait until
// the bytes are written. Swiftfin cancels Quick Connect at about half a second.
func (i *instance) fastResult(w http.ResponseWriter, r *http.Request, token string) {
	v, err := i.host.Identity.Resolve(frontend.WithQuietIdentity(r.Context()), token)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	jsonReply(w, map[string]any{"AccessToken": token, "ServerId": i.info.ID, "User": i.authUserDTO(v), "SessionInfo": i.authSessionDTO(r, v)})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	ctx := context.WithoutCancel(r.Context())
	viewer := *v
	dev := device(r)
	go i.finishSignIn(ctx, &viewer, dev)
}

func (i *instance) finishSignIn(ctx context.Context, v *frontend.Viewer, dev frontend.Device) {
	if v == nil {
		return
	}
	if note, ok := i.host.Identity.(interface {
		NoteSignIn(context.Context, int64) error
	}); ok {
		_ = note.NoteSignIn(ctx, v.ID)
	}
	if warm, ok := i.host.VirtualPaths.(interface {
		Warm(context.Context, *frontend.Viewer, frontend.Device)
	}); ok {
		warm.Warm(ctx, v, dev)
	}
}

func (i *instance) authUserDTO(v *frontend.Viewer) map[string]any {
	id, _ := ids.GUID(ids.User, v.ID)
	// Static keys only. Preferences, home order and capabilities are read on
	// later user requests so sign-in does not walk the catalogue.
	policy := map[string]any{
		"IsAdministrator": v.IsOwner && !v.Delegated, "IsHidden": true, "IsDisabled": false,
		"EnableCollectionManagement": false, "EnableSubtitleManagement": false, "EnableLyricManagement": false,
		"EnableUserPreferenceAccess": i.host.Preferences != nil, "EnableRemoteControlOfOtherUsers": false,
		"EnableSharedDeviceControl": false, "EnableRemoteAccess": false, "EnableLiveTvManagement": false, "EnableLiveTvAccess": false,
		"EnableMediaPlayback": i.host.Sessions != nil, "EnablePlaybackRemuxing": i.host.Sessions != nil,
		"EnableAudioPlaybackTranscoding": i.host.Sessions != nil, "EnableVideoPlaybackTranscoding": i.host.Sessions != nil,
		"ForceRemoteSourceTranscoding": false, "EnableContentDeletion": false, "EnableContentDownloading": false,
		"EnableSyncTranscoding": false, "EnableMediaConversion": false, "EnableAllDevices": true, "EnableAllChannels": true,
		"EnabledDevices": []string{}, "EnabledChannels": []string{}, "EnableContentDeletionFromFolders": []string{}, "AllowedTags": []string{}, "BlockedTags": []string{},
		"EnableAllFolders": len(v.AllowedSections) == 0, "InvalidLoginAttemptCount": 0, "LoginAttemptsBeforeLockout": -1,
		"MaxActiveSessions": v.StreamLimit, "EnablePublicSharing": false, "RemoteClientBitrateLimit": 0,
		"AuthenticationProviderId": "Ether", "PasswordResetProviderId": "Ether", "SyncPlayAccess": "None",
	}
	configuration := map[string]any{"PlayDefaultAudioTrack": true, "DisplayMissingEpisodes": false, "GroupedFolders": []string{}, "SubtitleMode": "Default", "DisplayCollectionsView": false, "EnableLocalPassword": false, "OrderedViews": []string{}, "LatestItemsExcludes": []string{}, "MyMediaExcludes": []string{}, "HidePlayedInLatest": true, "RememberAudioSelections": true, "RememberSubtitleSelections": true, "EnableNextEpisodeAutoPlay": true}
	if i.protocol == "emby" {
		for _, key := range []string{"AllowedTags", "EnableCollectionManagement", "EnableLyricManagement", "ForceRemoteSourceTranscoding", "LoginAttemptsBeforeLockout", "MaxActiveSessions", "PasswordResetProviderId", "SyncPlayAccess"} {
			delete(policy, key)
		}
		policy["SimultaneousStreamLimit"] = v.StreamLimit
		delete(configuration, "DisplayCollectionsView")
		delete(configuration, "GroupedFolders")
	}
	user := map[string]any{"Id": id, "Name": v.Username, "ServerId": i.info.ID, "HasPassword": true, "Policy": policy, "Configuration": configuration}
	if i.protocol == "jellyfin" {
		configuration["SubtitleLanguagePreference"] = ""
		configuration["CastReceiverId"] = ""
		user["EnableAutoLogin"] = false
		user["HasConfiguredEasyPassword"] = false
		policy["AccessSchedules"] = []any{}
		policy["BlockUnratedItems"] = []any{}
		policy["BlockedChannels"] = []any{}
		policy["BlockedMediaFolders"] = []any{}
	}
	if i.protocol == "emby" {
		i.embyUserDTO(user)
	}
	return user
}

func (i *instance) authSessionDTO(r *http.Request, v *frontend.Viewer) map[string]any {
	d := device(r)
	userID, _ := ids.GUID(ids.User, v.ID)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", i.protocol, v.ID, d.ID)))
	now := time.Now().UTC()
	state := map[string]any{"CanSeek": false, "IsPaused": false, "IsMuted": false, "RepeatMode": "RepeatNone"}
	session := map[string]any{"Id": hex.EncodeToString(sum[:16]), "UserId": userID, "UserName": v.Username, "ServerId": i.info.ID, "Client": d.Client, "DeviceId": d.ID, "DeviceName": d.DeviceName, "ApplicationVersion": d.Version, "LastActivityDate": wireTime(now), "RemoteEndPoint": "", "AdditionalUsers": []any{}, "PlayableMediaTypes": []any{}, "SupportedCommands": []any{}, "SupportsRemoteControl": false, "PlayState": state}
	if i.protocol == "jellyfin" {
		session["LastPlaybackCheckIn"] = wireTime(time.Time{})
		state["PlaybackOrder"] = "Default"
		session["IsActive"] = true
		session["HasCustomDeviceName"] = false
		session["SupportsMediaControl"] = false
		session["NowPlayingQueue"] = []any{}
		session["Capabilities"] = map[string]any{"PlayableMediaTypes": []any{}, "SupportedCommands": []any{}, "SupportsMediaControl": false, "SupportsPersistentIdentifier": d.ID != ""}
	} else {
		state["PlaybackRate"] = 1
		state["Shuffle"] = false
		state["SleepTimerMode"] = "None"
		state["SubtitleOffset"] = 0
		session["Protocol"] = r.Proto
		session["PlaylistIndex"] = 0
		session["PlaylistLength"] = 0
	}
	return session
}
func identityError(w http.ResponseWriter, err error) {
	if errors.Is(err, frontend.ErrRateLimited) {
		http.Error(w, "Too Many Requests", 429)
	} else {
		http.Error(w, "Unauthorized", 401)
	}
}
func (i *instance) quickEnabled() bool {
	if i.host.Config != nil {
		if value, ok := i.host.Config()["quickConnect"].(bool); ok {
			return value
		}
	}
	return true
}
func (i *instance) routes() error {
	add := func(method, path string, h http.HandlerFunc) error { return i.router.Handle(method, path, h) }
	routes := []struct {
		m, p string
		h    http.HandlerFunc
	}{
		{"GET", "/Sessions", i.sessionList},
		{"GET", "/System/Info", func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			jsonReply(w, map[string]any{"Id": i.info.ID, "ServerName": i.info.ServerName, "Version": i.info.Version, "ProductName": i.info.ProductName, "StartupWizardCompleted": true, "WebSocketPortNumber": i.listenerPort()})
		}},
		{"POST", "/Sessions/Capabilities", i.capabilitiesQuery},
		{"POST", "/Sessions/Capabilities/Full", func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			if i.host.Sessions == nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			var body struct {
				PlayableMediaTypes           []string
				SupportedCommands            []string
				SupportsMediaControl         *bool
				SupportsPersistentIdentifier *bool
				SupportsRemoteControl        *bool
				DeviceProfile                json.RawMessage
				AppStoreURL                  string `json:"AppStoreUrl"`
				IconURL                      string `json:"IconUrl"`
			}
			if decode(r, &body) != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			caps := frontend.SessionCapabilities{PlayableMediaTypes: body.PlayableMediaTypes, SupportedCommands: body.SupportedCommands, SupportsMediaControl: body.SupportsMediaControl, SupportsPersistentIdentifier: body.SupportsPersistentIdentifier, SupportsRemoteControl: body.SupportsRemoteControl, DeviceProfile: body.DeviceProfile, AppStoreURL: body.AppStoreURL, IconURL: body.IconURL}
			if err := i.host.Sessions.UpdateCapabilities(r.Context(), frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: i.protocol}, caps); err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			w.WriteHeader(204)
		}},
		{"GET", "/socket", i.socket},
		{"POST", "/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
			var body struct{ Username, Pw string }
			if decode(r, &body) != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			token, err := i.host.Identity.SignIn(i.identityContext(r), body.Username, body.Pw, device(r))
			if err != nil {
				identityError(w, err)
				return
			}
			i.fastResult(w, r, token)
		}},
		{"POST", "/Users/{userId}/Configuration", i.saveUserConfiguration},
		{"GET", "/Users/Me", func(w http.ResponseWriter, r *http.Request) {
			v, token, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			jsonReply(w, i.userDTOWithTimes(r, v, token))
		}},
		{"GET", "/Users/{userId}", func(w http.ResponseWriter, r *http.Request) {
			v, token, err := i.viewer(r)
			if errors.Is(err, errViewerScope) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			jsonReply(w, i.userDTOWithTimes(r, v, token))
		}},
		{"POST", "/Sessions/Logout", func(w http.ResponseWriter, r *http.Request) {
			_, token, err := i.viewer(r)
			if err == nil {
				err = i.host.Identity.Revoke(r.Context(), token)
			}
			if err != nil {
				identityError(w, err)
				return
			}
			w.WriteHeader(204)
		}},
		{"GET", "/QuickConnect/Enabled", func(w http.ResponseWriter, r *http.Request) { jsonReply(w, i.quickEnabled()) }},
		{"POST", "/QuickConnect/Initiate", func(w http.ResponseWriter, r *http.Request) {
			if !i.quickEnabled() {
				http.Error(w, "Quick Connect disabled", 401)
				return
			}
			dev := device(r)
			code, secret, err := i.host.Identity.QuickConnectInitiate(i.identityContext(r), dev)
			if err != nil {
				identityError(w, err)
				return
			}
			response := map[string]any{"Code": code, "Secret": secret, "Authenticated": false, "DeviceId": dev.ID, "DeviceName": dev.DeviceName, "AppName": dev.Client, "AppVersion": dev.Version, "DateAdded": wireTime(time.Now())}
			if i.host.ViewerPortalURL != nil {
				if portal, err := i.host.ViewerPortalURL(r.Context(), r); err == nil && portal != "" {
					response["VerificationUrl"] = portal
				}
			}
			jsonReply(w, response)
		}},
		{"GET", "/QuickConnect/Connect", func(w http.ResponseWriter, r *http.Request) {
			secret := route.Query(r.URL.Query(), "secret")
			if len(secret) != 1 || secret[0] == "" {
				http.Error(w, "Bad Request", 400)
				return
			}
			details, err := i.host.Identity.QuickConnectDetails(i.identityContext(r), secret[0])
			if err != nil {
				identityError(w, err)
				return
			}
			jsonReply(w, map[string]any{"Authenticated": details.Approved, "Secret": details.Secret, "Code": details.Code, "DeviceId": details.Device.ID, "DeviceName": details.Device.DeviceName, "AppName": details.Device.Client, "AppVersion": details.Device.Version, "DateAdded": wireTime(details.CreatedAt)})
		}},
		{"POST", "/QuickConnect/Authorize", func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			user := route.Query(r.URL.Query(), "userId")
			if len(user) > 0 {
				id, e := ids.ParseGUID(ids.User, user[0])
				if e != nil || id != v.ID {
					http.Error(w, "Forbidden", 403)
					return
				}
			}
			code := route.Query(r.URL.Query(), "code")
			if len(code) != 1 {
				http.Error(w, "Bad Request", 400)
				return
			}
			if err = i.host.Identity.QuickConnectApprove(i.identityContext(r), v, code[0]); err != nil {
				identityError(w, err)
				return
			}
			jsonReply(w, true)
		}},
		{"POST", "/Users/AuthenticateWithQuickConnect", func(w http.ResponseWriter, r *http.Request) {
			var body struct{ Secret string }
			if decode(r, &body) != nil || body.Secret == "" {
				http.Error(w, "Bad Request", 400)
				return
			}
			dev := device(r)
			var token string
			var err error
			if poller, ok := i.host.Identity.(interface {
				QuickConnectPollDevice(context.Context, string, frontend.Device) (string, error)
			}); ok {
				token, err = poller.QuickConnectPollDevice(i.identityContext(r), body.Secret, dev)
			} else {
				token, err = i.host.Identity.QuickConnectPoll(i.identityContext(r), body.Secret)
			}
			if err != nil {
				identityError(w, err)
				return
			}
			i.fastResult(w, r, token)
		}},
	}
	for _, r := range routes {
		if err := add(r.m, r.p, r.h); err != nil {
			return err
		}
	}
	return nil
}

func (i *instance) publicViewers(w http.ResponseWriter, r *http.Request) {
	viewers, err := i.host.Identity.PublicViewers(i.identityContext(r))
	if err != nil {
		identityError(w, err)
		return
	}
	items := make([]any, 0, len(viewers))
	for _, viewer := range viewers {
		id, e := ids.GUID(ids.User, viewer.ID)
		if e != nil {
			continue
		}
		item := map[string]any{"Id": id, "Name": viewer.Name, "ServerId": i.info.ID}
		if i.protocol == "emby" {
			if prefix := embyPrefix(viewer.Name); prefix != "" {
				item["Prefix"] = prefix
			}
		}
		if viewer.HasConfiguredPassword != nil {
			item["HasPassword"] = *viewer.HasConfiguredPassword
			if i.protocol == "emby" {
				item["HasConfiguredPassword"] = *viewer.HasConfiguredPassword
			}
		}
		items = append(items, item)
	}
	jsonReply(w, items)
}
