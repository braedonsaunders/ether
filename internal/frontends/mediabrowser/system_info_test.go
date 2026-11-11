package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi/spectest"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

// TestSystemInfoShape pins the extra reference fields and proves no real
// filesystem path is ever exposed on either protocol.
func TestSystemInfoShape(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1, Username: "viewer"}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	cases := []struct {
		name      string
		front     frontend.Frontend
		castArray bool
		spec      string
	}{
		{name: "jellyfin", front: Jellyfin{MachineID: "test"}, castArray: true, spec: "jellyfin"},
		{name: "emby", front: Emby{MachineID: "test"}, spec: "emby"},
	}
	forbidden := []string{"ProgramDataPath", "CachePath", "LogPath", "TranscodingTempPath", "InternalMetadataPath", "ItemsByNamePath", "WebPath", "EncoderLocation"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst, err := c.front.Mount(context.Background(), frontend.Host{Identity: identity, Sessions: frontendtest.Sessions{}})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/System/Info", nil)
			r.Header.Set("X-Emby-Token", "grant")
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var got map[string]any
			if json.Unmarshal(w.Body.Bytes(), &got) != nil {
				t.Fatal(w.Body.String())
			}
			for _, key := range forbidden {
				if _, ok := got[key]; ok {
					t.Fatalf("exposed %s", key)
				}
			}
			if got["HasPendingRestart"] != false || got["IsShuttingDown"] != false || got["SupportsLibraryMonitor"] != false {
				t.Fatal(got)
			}
			if _, ok := got["OperatingSystem"].(string); !ok || got["OperatingSystem"] == "" {
				t.Fatal("OperatingSystem", got["OperatingSystem"])
			}
			if c.castArray {
				if _, ok := got["CastReceiverApplications"].([]any); !ok {
					t.Fatal("CastReceiverApplications", got["CastReceiverApplications"])
				}
				if _, ok := got["SystemArchitecture"].(string); !ok {
					t.Fatal("SystemArchitecture", got["SystemArchitecture"])
				}
			} else if _, ok := got["LocalAddresses"].([]any); !ok {
				t.Fatal("LocalAddresses", got["LocalAddresses"])
			}
			if doc := spectest.Document(t, c.spec); doc != nil {
				if err := doc.Validate("GET", "/System/Info", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(w.Body.String(), "/Users/") || strings.Contains(w.Body.String(), "/home/") {
				t.Fatal("leaked path", w.Body.String())
			}
		})
	}
}
