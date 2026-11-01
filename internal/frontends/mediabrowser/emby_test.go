package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestEmbyOmitsJellyfinOnlyDTOFields(t *testing.T) {
	i := &instance{protocol: "emby"}
	user := i.userDTO(httptest.NewRequest("GET", "/Users/Me", nil), &frontend.Viewer{ID: 1})
	policy, config := user["Policy"].(map[string]any), user["Configuration"].(map[string]any)
	for _, key := range []string{"ForceRemoteSourceTranscoding", "LoginAttemptsBeforeLockout", "MaxActiveSessions", "PasswordResetProviderId", "SyncPlayAccess"} {
		if _, ok := policy[key]; ok {
			t.Fatal(key)
		}
	}
	if _, ok := config["GroupedFolders"]; ok {
		t.Fatal(config)
	}
	source := mediaSourceDTO("source", frontend.MediaInfo{Streams: []frontend.StreamInfo{{Type: "video"}}})
	i.mediaSourceDialect(source)
	for _, key := range []string{"GenPtsInput", "HasSegments", "IgnoreDts", "IgnoreIndex", "MediaAttachments"} {
		if _, ok := source[key]; ok {
			t.Fatal(key)
		}
	}
	if _, ok := source["MediaStreams"].([]any)[0].(map[string]any)["IsOriginal"]; ok {
		t.Fatal(source)
	}
}

func TestEmbySurfaceAndAuthentication(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1, Username: "alice"}
	ident := frontendtest.Identity{SignInFunc: func(_ context.Context, u, p string, d frontend.Device) (string, error) {
		if u != "alice" || p != "secret" || d.ID != "tv" {
			return "", errors.New("invalid")
		}
		return "emby-grant", nil
	}, ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "emby-grant" {
			return nil, errors.New("invalid")
		}
		return viewer, nil
	}}
	h, err := (Emby{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	j, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident})
	if err != nil {
		t.Fatal(err)
	}
	info := func(h frontend.Instance, path string) map[string]any {
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var x map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &x) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return x
	}
	e := info(h, "/emby/System/Info/Public")
	jf := info(j, "/jellyfin/System/Info/Public")
	if e["Version"] != "4.10.2.0" || e["Id"] == jf["Id"] {
		t.Fatal(e, jf)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"alice","pw":"secret"}`))
	r.Header.Set("Authorization", `Emby Client="TV", DeviceId="tv"`)
	h.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "emby-grant") {
		t.Fatal(w.Code, w.Body.String())
	}
	spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", "emby", "")
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate("POST", "/Users/AuthenticateByName", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/emby/System/Info/Public", nil))
	if err = doc.Validate("GET", "/System/Info/Public", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
}
