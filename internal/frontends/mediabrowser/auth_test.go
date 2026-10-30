package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestDeviceIDFromStreamQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/Videos/item/stream?DeviceId=tv", nil)
	if got := device(r).ID; got != "tv" {
		t.Fatal(got)
	}
	r.Header.Set("Authorization", `MediaBrowser DeviceId="header"`)
	if got := device(r).ID; got != "header" {
		t.Fatal(got)
	}
	r.Header.Del("Authorization")
	r = httptest.NewRequest("GET", "/Videos/item/stream?DeviceId=tv&deviceid=other", nil)
	if got := device(r).ID; got != "" {
		t.Fatal(got)
	}
}

func TestEmbyUserCreationDateIsIdentityOwned(t *testing.T) {
	created := time.Date(2024, 3, 2, 1, 2, 3, 0, time.UTC)
	i := &instance{protocol: "emby"}
	got := i.userDTO(httptest.NewRequest("GET", "/Users/Me", nil), &frontend.Viewer{ID: 1, CreatedAt: created})
	if got["DateCreated"] != wireTime(created) {
		t.Fatal(got)
	}
}

func TestUserHiddenPolicyMatchesPublicPicker(t *testing.T) {
	yes := true
	i := &instance{protocol: "jellyfin", host: frontend.Host{Config: func() frontend.Settings { return frontend.Settings{"publicViewerPicker": true} }, ViewerCapabilities: func(context.Context, *frontend.Viewer, frontend.Device, netip.Addr) (frontend.ViewerCapabilities, error) {
		return frontend.ViewerCapabilities{HasLocalPassword: &yes}, nil
	}}}
	got := i.userDTO(httptest.NewRequest("GET", "/Users/Me", nil), &frontend.Viewer{ID: 1})
	if got["Policy"].(map[string]any)["IsHidden"] != false {
		t.Fatal(got)
	}
}

func TestSessionCapabilitiesRoundTrip(t *testing.T) {
	v := &frontend.Viewer{ID: 9, Username: "viewer"}
	var saved frontend.SessionCapabilities
	sessions := frontendtest.Sessions{UpdateCapabilitiesFunc: func(_ context.Context, access frontend.SessionAccess, caps frontend.SessionCapabilities) error {
		if access.Viewer != v || access.Device.ID != "tv" || access.Protocol != "jellyfin" {
			t.Fatal(access)
		}
		saved = caps
		return nil
	}, CapabilitiesFunc: func(_ context.Context, access frontend.SessionAccess) (frontend.SessionCapabilities, error) {
		if access.Viewer != v || access.Device.ID != "tv" {
			t.Fatal(access)
		}
		return saved, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/Sessions/Capabilities/Full", strings.NewReader(`{"PlayableMediaTypes":["Video"],"SupportedCommands":["Pause"],"SupportsRemoteControl":true,"DeviceProfile":{"Name":"TV"}}`))
	r.Header.Set("Authorization", `MediaBrowser Token="valid", DeviceId="tv"`)
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 204 || saved.SupportsRemoteControl == nil || !*saved.SupportsRemoteControl {
		t.Fatal(w.Code, saved)
	}
	dto := inst.(*instance).sessionDTO(r, v)
	capDTO := dto["Capabilities"].(map[string]any)
	if dto["SupportsRemoteControl"] != true || capDTO["SupportsMediaControl"] != false || capDTO["SupportsPersistentIdentifier"] != false || dto["LastPlaybackCheckIn"] != wireTime(time.Time{}) || !strings.Contains(string(capDTO["DeviceProfile"].(json.RawMessage)), "TV") {
		t.Fatal(capDTO)
	}
}

func TestAuthentication(t *testing.T) {
	v := &frontend.Viewer{ID: 9, Username: "alice"}
	var revoked string
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token == "valid" {
			return v, nil
		}
		return nil, errors.New("invalid")
	}, SignInFunc: func(ctx context.Context, user, pw string, d frontend.Device) (string, error) {
		ip, ok := frontend.ClientIP(ctx)
		if !ok || ip.String() != "192.0.2.4" || user != "alice" || pw != "secret" || d.ID != "device" {
			return "", errors.New("invalid")
		}
		return "valid", nil
	}, RevokeFunc: func(_ context.Context, token string) error { revoked = token; return nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: frontendtest.Sessions{}, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.4"), true }})
	if err != nil {
		t.Fatal(err)
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	var doc *openapi.Document
	if e == nil {
		doc, e = openapi.Parse(spec)
	}
	if e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	cases := []struct {
		method, path, body, auth string
		status                   int
	}{
		{"POST", "/Users/AuthenticateByName", `{"Username":"alice","Pw":"secret"}`, `MediaBrowser Client="TV", DeviceId="device"`, 200},
		{"GET", "/Users/Me", "", `MediaBrowser Token="valid"`, 200},
		{"GET", "/System/Info", "", `MediaBrowser Token="valid"`, 200},
		{"POST", "/Sessions/Capabilities/Full", "{}", `MediaBrowser Token="valid"`, 204},
		{"GET", "/Users/Me", "", "", 401},
		{"GET", "/socket", "", "", 403},
		{"POST", "/Sessions/Logout", "", `MediaBrowser Token="valid"`, 204},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", c.auth)
		inst.Handler().ServeHTTP(w, r)
		if w.Code != c.status {
			t.Errorf("%s: %d %s", c.path, w.Code, w.Body.String())
			continue
		}
		if doc != nil && c.status == 200 {
			if err := doc.Validate(c.method, c.path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Errorf("%s: %v", c.path, err)
			}
		}
	}
	if revoked != "valid" {
		t.Fatal("grant not revoked")
	}
}

func TestJellyfinOwnedUserAndDTODefaults(t *testing.T) {
	v := &frontend.Viewer{ID: 9, Username: "alice", SubtitleLanguages: []string{"eng"}}
	ident := frontendtest.Identity{
		ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			if token == "valid" {
				return v, nil
			}
			return nil, errors.New("invalid")
		},
		SignInFunc: func(context.Context, string, string, frontend.Device) (string, error) { return "valid", nil },
	}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := ids.GUID(ids.User, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ids.GUID(ids.User, v.ID+1)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", `MediaBrowser Token="valid", DeviceId="device"`)
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	own := call("GET", "/Users/"+id+"?userId="+id, "")
	if own.Code != 200 || own.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatal(own.Code, own.Header().Get("Content-Type"), own.Body.String())
	}
	var user map[string]any
	if json.Unmarshal(own.Body.Bytes(), &user) != nil || user["Id"] != id || user["Name"] != "alice" {
		t.Fatal(own.Body.String())
	}
	if user["EnableAutoLogin"] != false || user["HasConfiguredEasyPassword"] != false {
		t.Fatal(user)
	}
	configuration := user["Configuration"].(map[string]any)
	if configuration["SubtitleLanguagePreference"] != "eng" || configuration["CastReceiverId"] != "" {
		t.Fatal(configuration)
	}
	policy := user["Policy"].(map[string]any)
	for _, key := range []string{"AccessSchedules", "BlockUnratedItems", "BlockedChannels", "BlockedMediaFolders"} {
		list, ok := policy[key].([]any)
		if !ok || len(list) != 0 {
			t.Fatalf("%s: %#v", key, policy[key])
		}
	}
	if denied := call("GET", "/Users/"+other, ""); denied.Code != 403 || strings.Contains(denied.Body.String(), "alice") {
		t.Fatal(denied.Code, denied.Body.String())
	}
	signed := call("POST", "/Users/AuthenticateByName", `{"Username":"alice","Pw":"secret"}`)
	if signed.Code != 200 || !strings.Contains(signed.Body.String(), `"EnableAutoLogin":false`) || !strings.Contains(signed.Body.String(), `"CastReceiverId":""`) {
		t.Fatal(signed.Code, signed.Body.String())
	}
}

func TestQuickConnectStatusDoesNotRedeem(t *testing.T) {
	approved := false
	redeemed := 0
	ident := frontendtest.Identity{
		ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			if token == "grant" {
				return &frontend.Viewer{ID: 1, Username: "viewer"}, nil
			}
			return nil, errors.New("bad grant")
		},
		InitiateFunc: func(context.Context, frontend.Device) (string, string, error) { return "123456", "secret", nil },
		DetailsFunc: func(_ context.Context, secret string) (frontend.QuickConnectDetails, error) {
			if secret != "secret" {
				return frontend.QuickConnectDetails{}, errors.New("invalid")
			}
			return frontend.QuickConnectDetails{Approved: approved, Secret: secret, Code: "123456", CreatedAt: time.Now()}, nil
		},
		ApproveFunc: func(_ context.Context, v *frontend.Viewer, code string) error {
			if v.ID != 1 || code != "123456" {
				return errors.New("invalid")
			}
			approved = true
			return nil
		},
		PollFunc: func(_ context.Context, secret string) (string, error) {
			if secret != "secret" || !approved || redeemed != 0 {
				return "", errors.New("invalid")
			}
			redeemed++
			return "grant", nil
		},
	}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	spec, specErr := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	var doc *openapi.Document
	if specErr == nil {
		doc, specErr = openapi.Parse(spec)
	}
	if specErr != nil && !os.IsNotExist(specErr) {
		t.Fatal(specErr)
	}
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Emby-Token", token)
		inst.Handler().ServeHTTP(w, r)
		if doc != nil && w.Code == 200 {
			if e := doc.Validate(method, path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
				t.Errorf("%s: %v", path, e)
			}
		}
		return w
	}
	if w := call("POST", "/QuickConnect/Initiate", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"Code":"123456"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("GET", "/QuickConnect/Connect?secret=secret", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"Authenticated":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("POST", "/QuickConnect/Authorize?code=123456", "", "grant"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for j := 0; j < 2; j++ {
		if w := call("GET", "/QuickConnect/Connect?secret=secret", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"Authenticated":true`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if redeemed != 0 {
		t.Fatal("status consumed grant")
	}
	if w := call("POST", "/Users/AuthenticateWithQuickConnect", `{"Secret":"secret"}`, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if redeemed != 1 {
		t.Fatal(redeemed)
	}
}

func TestQuickConnectOwnerSetting(t *testing.T) {
	enabled := false
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{}, Config: func() frontend.Settings { return frontend.Settings{"quickConnect": enabled} }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	if w := call("GET", "/QuickConnect/Enabled"); w.Body.String() != "false\n" {
		t.Fatal(w.Body.String())
	}
	if w := call("POST", "/QuickConnect/Initiate"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	enabled = true
	if w := call("GET", "/QuickConnect/Enabled"); w.Body.String() != "true\n" {
		t.Fatal(w.Body.String())
	}
}

func TestLegacyDeviceHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Emby-Client", "App")
	r.Header.Set("X-Emby-Device-Id", "stable-id")
	r.Header.Set("X-Emby-Device-Name", "TV")
	r.Header.Set("X-Emby-Client-Version", "1.0")
	d := device(r)
	if d.ID != "stable-id" || d.Client != "App" || d.DeviceName != "TV" || d.Version != "1.0" {
		t.Fatal(d)
	}
	r.Header.Set("Authorization", `Emby Client="Other", DeviceId="header-id"`)
	d = device(r)
	if d.ID != "header-id" || d.Client != "Other" || d.DeviceName != "TV" {
		t.Fatal(d)
	}
}

func TestPublicViewerPickerUsesVerifiedPeer(t *testing.T) {
	ident := frontendtest.Identity{PublicViewersFunc: func(ctx context.Context) ([]frontend.PublicViewer, error) {
		ip, ok := frontend.ClientIP(ctx)
		if !ok || ip.String() != "192.0.2.4" {
			return nil, errors.New("missing peer")
		}
		return []frontend.PublicViewer{{ID: 3, Name: "alice"}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.4"), true }})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/Users/Public", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Name":"alice"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestViewerPolicyUsesAuthoritativeCapabilities(t *testing.T) {
	viewer := &frontend.Viewer{ID: 3, Username: "alice"}
	no := false
	yes := true
	bitrate := 5000
	limit := 3
	blocked := []string{"restricted"}
	views := []frontend.View{{ID: "1", Name: "Movies"}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	host := frontend.Host{Identity: ident, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("198.51.100.7"), true }, ViewerCapabilities: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, ip netip.Addr) (frontend.ViewerCapabilities, error) {
		if v != viewer || ip.String() != "198.51.100.7" {
			t.Fatal(v, ip)
		}
		return frontend.ViewerCapabilities{CanPlay: &no, CanPlayRemote: &no, CanUseLiveTV: &yes, CanDownload: &yes, CanTranscodeAudio: &yes, CanTranscodeVideo: &no, CanRemux: &yes, MaxActiveSessions: &limit, BlockedTags: &blocked, RemoteBitrateKbps: &bitrate, HasLocalPassword: &yes, AllowedLibraries: &views}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/Users/Me", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	var result struct {
		HasPassword bool
		Policy      map[string]any
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.HasPassword || result.Policy["EnableRemoteAccess"] != false || result.Policy["EnableLiveTvAccess"] != true || result.Policy["RemoteClientBitrateLimit"] != float64(5000000) {
		t.Fatal(w.Code, w.Body.String())
	}
	if result.Policy["EnableVideoPlaybackTranscoding"] != false {
		t.Fatal("video transcoding policy was not honored")
	}
	if result.Policy["EnableContentDownloading"] != false {
		t.Fatal("MediaBrowser downloads are not supported")
	}
	if result.Policy["IsHidden"] != true || result.Policy["EnableRemoteControlOfOtherUsers"] != false {
		t.Fatal(result.Policy)
	}
	if result.Policy["EnableMediaPlayback"] != false || result.Policy["EnableAudioPlaybackTranscoding"] != true || result.Policy["EnablePlaybackRemuxing"] != true || result.Policy["MaxActiveSessions"] != float64(3) || len(result.Policy["BlockedTags"].([]any)) != 1 {
		t.Fatal(result.Policy)
	}
}
