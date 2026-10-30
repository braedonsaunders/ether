package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestPublicPickerUsesExplicitPasswordCapability(t *testing.T) {
	for _, protocol := range []frontend.Protocol{"emby", "jellyfin"} {
		t.Run(string(protocol), func(t *testing.T) {
			yes, no := true, false
			identity := frontendtest.Identity{PublicViewersFunc: func(context.Context) ([]frontend.PublicViewer, error) {
				return []frontend.PublicViewer{{ID: 1, Name: "known", HasConfiguredPassword: &yes}, {ID: 2, Name: "none", HasConfiguredPassword: &no}, {ID: 3, Name: "unknown"}}, nil
			}}
			inst, err := mount(context.Background(), frontend.Host{Identity: identity}, "test", protocol)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/Users/Public", nil))
			var users []map[string]any
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &users) != nil || len(users) != 3 {
				t.Fatal(w.Code, w.Body.String())
			}
			if users[0]["HasPassword"] != true || users[1]["HasPassword"] != false {
				t.Fatal(users)
			}
			if _, ok := users[2]["HasPassword"]; ok {
				t.Fatal("unknown password capability was invented", users[2])
			}
			if protocol == "emby" {
				if users[0]["HasConfiguredPassword"] != true || users[1]["HasConfiguredPassword"] != false {
					t.Fatal(users)
				}
				if _, ok := users[2]["HasConfiguredPassword"]; ok {
					t.Fatal(users[2])
				}
				if users[0]["Prefix"] != "K" || users[1]["Prefix"] != "N" || users[2]["Prefix"] != "U" {
					t.Fatal(users)
				}
			} else if _, ok := users[0]["HasConfiguredPassword"]; ok || users[0]["Prefix"] != nil {
				t.Fatal(users[0])
			}
			for _, user := range users {
				if _, ok := user["LastLoginDate"]; ok {
					t.Fatal("public login history", user)
				}
				if _, ok := user["LastActivityDate"]; ok {
					t.Fatal("public activity history", user)
				}
			}
		})
	}
}
