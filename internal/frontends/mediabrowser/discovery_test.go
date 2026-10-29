package mediabrowser

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestDiscoveryResponderUsesCorrectSurface(t *testing.T) {
	for _, tc := range []struct {
		protocol               frontend.Protocol
		probe, other, endpoint string
	}{
		{"jellyfin", "who is JellyfinServer?", "who is EmbyServer?", "http://192.0.2.10:8096"},
		{"emby", "Who is EmbyServer?", "who is JellyfinServer?", "http://192.0.2.10:8097/emby"},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			settings := frontend.Settings{"serverName": "Configured"}
			i := &instance{protocol: tc.protocol, info: PublicInfo{ID: string(tc.protocol) + "-id", ServerName: "Ether"}, host: frontend.Host{Config: func() frontend.Settings { return settings }}}
			if len(i.Discovery()) != 0 {
				t.Fatal("per-surface UDP listener still enabled")
			}
			req := frontend.DiscoveryRequest{Data: []byte(tc.probe), Endpoint: tc.endpoint}
			data, ok := i.RespondDiscovery(context.Background(), req)
			if !ok {
				t.Fatal("probe rejected")
			}
			var got map[string]string
			if json.Unmarshal(data, &got) != nil || got["Address"] != tc.endpoint || got["Id"] != string(tc.protocol)+"-id" || got["Name"] != "Configured" {
				t.Fatal(got)
			}
			if tc.protocol == "jellyfin" && got["EndpointAddress"] != "192.0.2.10" {
				t.Fatal(got)
			}
			if tc.protocol == "emby" && got["EndpointAddress"] != "" {
				t.Fatal(got)
			}
			req.Data = []byte(tc.other)
			if _, ok = i.RespondDiscovery(context.Background(), req); ok {
				t.Fatal("answered another surface probe")
			}
			req.Data = []byte(tc.probe)
			settings["discovery"] = false
			if _, ok = i.RespondDiscovery(context.Background(), req); ok {
				t.Fatal("answered disabled surface")
			}
			settings["discovery"] = true
			req.Endpoint = "http://user:secret@example.invalid/"
			if _, ok = i.RespondDiscovery(context.Background(), req); ok {
				t.Fatal("accepted invalid endpoint")
			}
		})
	}
}
