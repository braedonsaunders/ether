package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestStreamMetadataMountedSourceFacts(t *testing.T) {
	// The measured target receipt establishes these facts only: no codec tags,
	// DAR/SAR, progressive scan or SDR were included in that receipt.
	target := []frontend.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 320, Height: 180, FrameRate: 24},
		{Index: 1, Type: "audio", Codec: "aac", SampleRate: 48000, Channels: 1},
	}
	// Separate synthetic resolved-source facts exercise optional probe metadata
	// and multiple tracks. They are not attributed to the measured target.
	multiple := []frontend.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", CodecTag: "avc1", Width: 720, Height: 480, SampleAspectRatio: "8:9"},
		{Index: 2, Type: "audio", Codec: "aac", CodecTag: "mp4a", Channels: 1, SampleRate: 48000},
		{Index: 5, Type: "audio", Codec: "opus", Channels: 2, Title: "Commentary"},
		{Index: 7, Type: "subtitle", Codec: "subrip", Title: "English captions"},
	}
	for _, protocol := range []string{"jellyfin", "emby"} {
		for _, tc := range []struct {
			name                 string
			streams              []frontend.StreamInfo
			estimated            bool
			titles, tags, ratios []string
		}{
			{"measured target facts", target, false, []string{"H264 - 320x180", "AAC - 1 ch - 48000 Hz"}, []string{"", ""}, []string{"", ""}},
			{"multiple resolved tracks", multiple, false, []string{"H264 - 720x480", "AAC - 1 ch - 48000 Hz", "Commentary - OPUS - 2 ch", "English captions - SUBRIP"}, []string{"avc1", "mp4a", "", ""}, []string{"4:3", "", "", ""}},
			{"unknown", []frontend.StreamInfo{{Index: 3, Type: "video"}}, false, []string{""}, []string{""}, []string{""}},
			{"estimated", multiple, true, nil, nil, nil},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				viewer := &frontend.Viewer{ID: 1}
				media := frontend.MediaInfo{Container: "mp4", Streams: tc.streams, Estimated: tc.estimated}
				before, _ := json.Marshal(media)
				identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
				experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
					return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}, Tracks: tc.streams, TracksEstimated: tc.estimated, Versions: []frontend.Version{{ID: "private-release", Media: media}}}, nil
				}}
				const handle = "4e0f0a56-014b-4883-89b7-e9465f03462e"
				plans := 0
				sessions := frontendtest.Sessions{PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
					plans++
					if req.Item != 5 || req.Version != handle || !req.Intent.ExactVersion || req.Viewer.ID != 1 || req.Device.ID != "device" || string(req.Protocol) != protocol {
						t.Fatalf("source scope changed: %#v", req)
					}
					return frontend.PlayPlan{Media: media, Estimated: tc.estimated, ETag: "source-revision-7", DirectPlay: true}, nil
				}}
				var front frontend.Frontend = Jellyfin{MachineID: "test"}
				if protocol == "emby" {
					front = Emby{MachineID: "test"}
				}
				inst, err := front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: sessions, Capabilities: frontendtest.Capabilities{}, VersionHandles: &testVersionHandles{item: 5, handle: handle}})
				if err != nil {
					t.Fatal(err)
				}
				check := func(raw any) {
					t.Helper()
					streams, ok := raw.([]any)
					if !ok || len(streams) != len(tc.titles) {
						t.Fatalf("unexpected streams: %#v", raw)
					}
					for n, raw := range streams {
						dto := raw.(map[string]any)
						if dto["Index"] != float64(tc.streams[n].Index) {
							t.Fatal("track index changed", dto)
						}
						for field, want := range map[string]string{"AspectRatio": tc.ratios[n], "CodecTag": tc.tags[n], "DisplayTitle": tc.titles[n]} {
							got, present := dto[field]
							if (want == "" && present) || (want != "" && got != want) {
								t.Errorf("track %d %s = %#v, want %q (empty means absent)", n, field, got, want)
							}
						}
					}
				}
				for _, playbackInfo := range []bool{false, true} {
					method, path, body := "GET", "/Items/"+ids.RatingKeyGUID(5), ""
					if protocol == "emby" && !playbackInfo {
						user, _ := ids.GUID(ids.User, viewer.ID)
						path = "/Users/" + user + path
					}
					if playbackInfo {
						method = "POST"
						path = "/Items/" + ids.RatingKeyGUID(5) + "/PlaybackInfo"
						body = `{"IsPlayback":false,"MediaSourceId":"` + handle + `"}`
					}
					r := httptest.NewRequest(method, path, strings.NewReader(body))
					r.Header.Set("X-Emby-Token", "grant")
					r.Header.Set("X-Emby-Device-Id", "device")
					r.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					inst.Handler().ServeHTTP(w, r)
					var dto map[string]any
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &dto) != nil {
						t.Fatal(w.Code, w.Body.String())
					}
					sources := dto["MediaSources"].([]any)
					if len(sources) != 1 {
						t.Fatal("source selection changed", sources)
					}
					source := sources[0].(map[string]any)
					if source["Id"] != handle {
						t.Fatal("source binding changed", source)
					}
					check(source["MediaStreams"])
					if playbackInfo {
						if source["ETag"] != "source-revision-7" || source["SupportsDirectPlay"] != true {
							t.Fatal("broker facts changed", source)
						}
					} else if tc.estimated {
						if _, present := dto["MediaStreams"]; present {
							t.Fatal("estimated top-level tracks exposed", dto)
						}
					} else {
						check(dto["MediaStreams"])
					}
				}
				if plans != 1 {
					t.Fatal("unexpected plan calls", plans)
				}
				after, _ := json.Marshal(media)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("source facts mutated")
				}
			})
		}
	}
}

func TestStreamMetadataAspectEvidence(t *testing.T) {
	for _, dialect := range []frontend.Protocol{"jellyfin", "emby"} {
		for _, tc := range []struct {
			name   string
			stream frontend.StreamInfo
			want   string
		}{
			{"explicit DAR wins", frontend.StreamInfo{Type: "video", Width: 720, Height: 480, SampleAspectRatio: "8:9", DisplayAspectRatio: "16:9"}, "16:9"},
			{"square pixels", frontend.StreamInfo{Type: "video", Width: 320, Height: 180, SampleAspectRatio: "1:1"}, "16:9"},
			{"unknown SAR", frontend.StreamInfo{Type: "video", Width: 320, Height: 180}, ""},
			{"zero SAR", frontend.StreamInfo{Type: "video", Width: 320, Height: 180, SampleAspectRatio: "0:1"}, ""},
			{"zero divisor", frontend.StreamInfo{Type: "video", Width: 320, Height: 180, SampleAspectRatio: "1:0"}, ""},
			{"invalid SAR", frontend.StreamInfo{Type: "video", Width: 320, Height: 180, SampleAspectRatio: "N/A"}, ""},
			{"negative SAR", frontend.StreamInfo{Type: "video", Width: 320, Height: 180, SampleAspectRatio: "-1:1"}, ""},
			{"oversized SAR", frontend.StreamInfo{Type: "video", Width: 320, Height: 180, SampleAspectRatio: "100001:1"}, ""},
			{"unknown height", frontend.StreamInfo{Type: "video", Width: 320, SampleAspectRatio: "1:1"}, ""},
		} {
			t.Run(string(dialect)+"/"+tc.name, func(t *testing.T) {
				dto := (&instance{protocol: dialect}).mediaStreams([]frontend.StreamInfo{tc.stream})[0].(map[string]any)
				got, present := dto["AspectRatio"]
				if (tc.want == "" && present) || (tc.want != "" && got != tc.want) {
					t.Fatalf("AspectRatio=%#v, want %q", got, tc.want)
				}
			})
		}
	}
}
