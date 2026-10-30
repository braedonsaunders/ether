package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestDetailSourceOmitsEstimatedMediaFacts(t *testing.T) {
	media := frontend.MediaInfo{Estimated: true, Container: "mkv", Size: 1000, BitrateKbps: 5000, Duration: time.Hour, Streams: []frontend.StreamInfo{{Type: "video", Codec: "h264", Width: 1920}}}
	for _, dialect := range []string{"jellyfin", "emby"} {
		i := &instance{protocol: frontend.Protocol(dialect)}
		got := i.detailMediaSourceDTO("scoped", media)
		for _, field := range []string{"Container", "RunTimeTicks", "Size", "Bitrate", "Chapters"} {
			if _, ok := got[field]; ok {
				t.Fatalf("%s: estimated %s exposed: %#v", dialect, field, got[field])
			}
		}
		if streams, ok := got["MediaStreams"].([]any); !ok || len(streams) != 0 {
			t.Fatalf("%s: streams = %#v", dialect, got["MediaStreams"])
		}
		media.Estimated = false
		actual := i.detailMediaSourceDTO("scoped", media)
		if actual["Container"] != "mkv" || actual["Size"] != int64(1000) || len(actual["MediaStreams"].([]any)) != 1 {
			t.Fatalf("%s: authoritative source lost: %#v", dialect, actual)
		}
		media.Estimated = true
	}
}

func TestItemDetailDoesNotExposeEstimatedVersionStreams(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}, Versions: []frontend.Version{{ID: "version", Media: frontend.MediaInfo{Estimated: true, Container: "mkv", Size: 42, Streams: []frontend.StreamInfo{{Type: "video", Width: 1920}}}}}}, nil
	}}
	for _, protocol := range []frontend.Protocol{"jellyfin", "emby"} {
		var front frontend.Frontend
		if protocol == "jellyfin" {
			front = Jellyfin{MachineID: "test"}
		} else {
			front = Emby{MachineID: "test"}
		}
		inst, err := front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience})
		if err != nil {
			t.Fatal(err)
		}
		path := "/Items/" + ids.RatingKeyGUID(5)
		if protocol == "emby" {
			userID, _ := ids.GUID(ids.User, viewer.ID)
			path = "/Users/" + userID + path
		}
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		var dto map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &dto) != nil {
			t.Fatal(protocol, w.Code, w.Body.String())
		}
		sources := dto["MediaSources"].([]any)
		source := sources[0].(map[string]any)
		if _, ok := source["Container"]; ok || len(source["MediaStreams"].([]any)) != 0 {
			t.Fatal(protocol, source)
		}
		spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", string(protocol), "")
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		doc, err := openapi.Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := doc.Validate("GET", r.URL.Path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
			t.Fatalf("%s: %v", protocol, err)
		}
	}
}

func TestEmbyDetailOmitsEstimatedVersionFacts(t *testing.T) {
	parts := 1
	detail := frontend.ItemDetail{Versions: []frontend.Version{{FileName: "movie.mkv", PartCount: &parts, Media: frontend.MediaInfo{Estimated: true, Container: "mkv", Size: 1000, BitrateKbps: 5000, Streams: []frontend.StreamInfo{{Type: "video", Width: 1920, Height: 1080}}}}}}
	got := map[string]any{}
	(&instance{protocol: "emby"}).embyItemDetailDTO(got, detail)
	if got["FileName"] != "movie.mkv" || got["PartCount"] != 1 {
		t.Fatal(got)
	}
	for _, field := range []string{"Container", "Size", "Bitrate", "Width", "Height", "Chapters"} {
		if _, ok := got[field]; ok {
			t.Fatalf("estimated %s exposed: %#v", field, got[field])
		}
	}
	source := map[string]any{}
	(&instance{protocol: "emby"}).embyMediaSourceDTO(source, "item", detail.Versions[0].Media)
	if _, ok := source["MimeType"]; ok {
		t.Fatal("estimated source MIME type exposed", source)
	}
}
