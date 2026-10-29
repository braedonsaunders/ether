package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

// The Kotlin generator treats non-nullable value types without defaults as
// required, even when OpenAPI's required list omits them. Derive those fields
// from the pinned schema so newly added SDK requirements fail this test.
func TestJellyfinAuthenticationStrictRequiredFields(t *testing.T) {
	spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if os.IsNotExist(err) {
		t.Skip("pinned Jellyfin spec is not cached")
	}
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Type     string          `json:"type"`
					Format   string          `json:"format"`
					Nullable bool            `json:"nullable"`
					Default  json.RawMessage `json:"default"`
					Enum     []any           `json:"enum"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(spec, &raw); err != nil {
		t.Fatal(err)
	}
	viewer := &frontend.Viewer{ID: 1, Username: "viewer"}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }, SignInFunc: func(context.Context, string, string, frontend.Device) (string, error) { return "grant", nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	check := func(schema string, value any) {
		t.Helper()
		object, ok := value.(map[string]any)
		if !ok {
			t.Errorf("%s is not an object", schema)
			return
		}
		definition := raw.Components.Schemas[schema]
		required := map[string]bool{}
		for _, key := range definition.Required {
			required[key] = true
		}
		for key, property := range definition.Properties {
			if property.Nullable || len(property.Default) > 0 {
				continue
			}
			switch property.Type {
			case "boolean", "integer", "number", "array":
				required[key] = true
			case "string":
				if property.Format == "uuid" || property.Format == "date-time" {
					required[key] = true
				}
			}
			if len(property.Enum) > 0 {
				required[key] = true
			}
		}
		for key := range required {
			value, exists := object[key]
			if !exists || value == nil {
				t.Errorf("%s.%s missing or null", schema, key)
			}
		}
	}
	for _, endpoint := range []struct{ method, path, body string }{{"POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"password"}`}, {"GET", "/Users/Me", ""}} {
		r := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(endpoint.body))
		r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="device"`)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", endpoint.path, w.Code, w.Body.String())
		}
		if err := doc.Validate(endpoint.method, endpoint.path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		var root map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
			t.Fatal(err)
		}
		user := root
		if endpoint.method == "POST" {
			user, _ = root["User"].(map[string]any)
			session, _ := root["SessionInfo"].(map[string]any)
			check("SessionInfoDto", session)
			check("ClientCapabilitiesDto", session["Capabilities"])
			check("PlayerStateInfo", session["PlayState"])
		}
		check("UserDto", user)
		check("UserPolicy", user["Policy"])
		check("UserConfiguration", user["Configuration"])
	}
}
