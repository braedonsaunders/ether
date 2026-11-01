package mediabrowser

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type channelGrants struct{ requests []frontend.ImageGrantRequest }

func (g *channelGrants) Issue(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
	g.requests = append(g.requests, req)
	return "channel-grant", nil
}
func (*channelGrants) Revoke(context.Context, string, string) error { return nil }
func (*channelGrants) Serve(w http.ResponseWriter, _ *http.Request, _ string) {
	w.WriteHeader(http.StatusNoContent)
}

func TestLiveChannelImageUsesChannelScopedGrant(t *testing.T) {
	grants := &channelGrants{}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 1}, nil }}
	live := frontendtest.LiveTV{ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
		return []frontend.LiveChannel{{ID: "channel-1", Name: "News", Images: []frontend.Image{{Kind: frontend.ImageChannel, Path: "opaque"}}}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, LiveTV: live, ImageGrants: grants})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/LiveTv/Channels", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "channel-grant") || len(grants.requests) != 1 || grants.requests[0].Channel != "channel-1" || grants.requests[0].Item != 0 || grants.requests[0].Credential != "grant" {
		t.Fatal(w.Code, w.Body.String(), grants.requests)
	}
}

func TestLiveTVUsesViewerScopedHost(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	id, _ := ids.StringGUID(ids.Channel, "channel-1")
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	live := frontendtest.LiveTV{ChannelsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.LiveChannel, error) {
		if v != viewer {
			t.Fatal(v)
		}
		return []frontend.LiveChannel{{ID: "channel-1", Name: "News", Number: "2"}}, nil
	}, ResolveChannelFunc: func(_ context.Context, v *frontend.Viewer, guid string) (frontend.LiveChannel, error) {
		if v != viewer || guid != id {
			t.Fatal(v, guid)
		}
		return frontend.LiveChannel{ID: "channel-1"}, nil
	}, GuideFunc: func(_ context.Context, v *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
		if v != viewer || len(q.Channels) != 1 || q.Channels[0] != "channel-1" || !q.From.Equal(start) || q.Device.ID != "guide-device" {
			t.Fatal(v, q)
		}
		return []frontend.LiveProgramme{{ID: "program-1", ChannelID: "channel-1", Title: "News", Start: start, End: start.Add(time.Hour)}}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, LiveTV: live})
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
	paths := []string{"/LiveTv/Channels", "/LiveTv/Programs?ChannelIds=" + id + "&MinStartDate=" + url.QueryEscape(start.Format(time.RFC3339)) + "&MaxStartDate=" + url.QueryEscape(start.Add(time.Hour).Format(time.RFC3339))}
	for _, path := range paths {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		r.Header.Set("Authorization", `MediaBrowser DeviceId="guide-device"`)
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "News") {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if strings.HasPrefix(path, "/LiveTv/Programs") && !strings.Contains(w.Body.String(), `"StartDate":"2026-01-01T12:00:00.0000000Z"`) {
			t.Fatal("programme timestamp precision", w.Body.String())
		}
		if doc != nil {
			if err := doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Fatal(path, err)
			}
		}
	}
}

func TestLivePlaybackUsesAuthorizedChannelTarget(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	guid, _ := ids.StringGUID(ids.Channel, "channel-1")
	var estimate, opened bool
	live := frontendtest.LiveTV{ResolveChannelFunc: func(_ context.Context, v *frontend.Viewer, id string) (frontend.LiveChannel, error) {
		if v != viewer || id != guid {
			t.Fatal(v, id)
		}
		return frontend.LiveChannel{ID: "channel-1"}, nil
	}}
	broker := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		if r.Channel != "channel-1" || r.Item != 0 {
			t.Fatal(r)
		}
		estimate = r.Intent.Estimate
		return frontend.PlayPlan{ID: "plan", Estimated: estimate}, nil
	}, OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
		opened = true
		return frontend.Session{ID: "session"}, nil
	}, StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
		if r.Format != frontend.StreamHLS {
			t.Fatal(r)
		}
		return "/sessions/media/session/scoped/index.m3u8", nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, LiveTV: live, Sessions: broker, Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/Items/"+guid+"/PlaybackInfo", strings.NewReader(body))
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	if w := call(`{"IsPlayback":false}`); w.Code != 200 || !estimate || opened {
		t.Fatal(w.Code, estimate, opened)
	}
	if w := call(`{"IsPlayback":true}`); w.Code != 200 || estimate || !opened || !strings.Contains(w.Body.String(), "/sessions/media/") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLiveTVInfoViewChannelAndOnNow(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	now := time.Now().UTC()
	live := frontendtest.LiveTV{
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{{ID: "channel-1", Name: "News", Number: "2"}}, nil
		},
		ResolveChannelFunc: func(_ context.Context, _ *frontend.Viewer, guid string) (frontend.LiveChannel, error) {
			id, _ := ids.StringGUID(ids.Channel, "channel-1")
			if guid != id {
				t.Fatal(guid)
			}
			return frontend.LiveChannel{ID: "channel-1", Name: "News", Number: "2"}, nil
		},
		GuideFunc: func(context.Context, *frontend.Viewer, frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
			return []frontend.LiveProgramme{
				{ID: "now", ChannelID: "channel-1", Title: "On Now", Start: now.Add(-time.Minute), End: now.Add(time.Hour)},
				{ID: "later", ChannelID: "channel-1", Title: "Later", Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
			}, nil
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	exp := frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
	}}
	for _, front := range []frontend.Frontend{Jellyfin{MachineID: "test"}, Emby{MachineID: "test"}} {
		inst, err := front.Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, LiveTV: live})
		if err != nil {
			t.Fatal(err)
		}
		call := func(path string) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("X-Emby-Token", "grant")
			inst.Handler().ServeHTTP(w, r)
			return w
		}
		info := call("/LiveTv/Info")
		if info.Code != 200 || !strings.Contains(info.Body.String(), `"IsEnabled":true`) || !strings.Contains(info.Body.String(), `"EnabledUsers":[]`) || !strings.Contains(info.Body.String(), `"Name":"Ether"`) || !strings.Contains(info.Body.String(), `"IsVisible":true`) {
			t.Fatal(front.Describe().ID, info.Code, info.Body.String())
		}
		views := call("/UserViews")
		if views.Code != 200 || !strings.Contains(views.Body.String(), `"CollectionType":"livetv"`) || !strings.Contains(views.Body.String(), `"Name":"Live TV"`) {
			t.Fatal(front.Describe().ID, views.Code, views.Body.String())
		}
		id, _ := ids.StringGUID(ids.Channel, "channel-1")
		one := call("/LiveTv/Channels/" + id)
		if one.Code != 200 || !strings.Contains(one.Body.String(), `"Name":"News"`) || !strings.Contains(one.Body.String(), `"Type":"TvChannel"`) || strings.Contains(one.Body.String(), `"Items"`) {
			t.Fatal(front.Describe().ID, one.Code, one.Body.String())
		}
		rec := call("/LiveTv/Programs/Recommended?IsAiring=true&Limit=5")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"Name":"On Now"`) || strings.Contains(rec.Body.String(), "Later") || !strings.Contains(rec.Body.String(), `"ChannelName":"News"`) || !strings.Contains(rec.Body.String(), `"TotalRecordCount":1`) {
			t.Fatal(front.Describe().ID, rec.Code, rec.Body.String())
		}
	}
}

func TestSwiftfinLiveTVChannelsProgramsAndTune(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	progID, err := ids.StringGUID(ids.Programme, "abc-news")
	if err != nil {
		t.Fatal(err)
	}
	channelID, err := ids.StringGUID(ids.Channel, "abc")
	if err != nil {
		t.Fatal(err)
	}
	virtualID, err := ids.StringGUID(ids.Channel, "virtual")
	if err != nil {
		t.Fatal(err)
	}
	userID, err := ids.GUID(ids.User, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	var libraryCalls int
	var tuned, stoppedID string
	live := frontendtest.LiveTV{
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{
				{ID: "virtual", Name: "Virtual", Number: "9", Kind: "virtual"},
				{ID: "abc", Name: "ABC10", Number: "10", Kind: "iptv"},
			}, nil
		},
		ResolveChannelFunc: func(_ context.Context, _ *frontend.Viewer, id string) (frontend.LiveChannel, error) {
			if id != channelID {
				return frontend.LiveChannel{}, core.ErrNotFound
			}
			return frontend.LiveChannel{ID: "abc", Name: "ABC10", Number: "10", Kind: "iptv"}, nil
		},
		ResolveProgrammeFunc: func(_ context.Context, _ *frontend.Viewer, id string, q frontend.LiveGuideQuery) (frontend.LiveProgramme, error) {
			if id != progID || !q.Until.After(q.From) || q.Until.Sub(q.From) > 48*time.Hour {
				t.Fatal(id, q.From, q.Until)
			}
			return frontend.LiveProgramme{ID: "abc-news", ChannelID: "abc", Title: "ABC10 News", Start: start, End: end}, nil
		},
	}
	exp := frontendtest.Experience{LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (core.Page, error) {
		libraryCalls++
		return core.Page{Items: []frontend.Title{{Key: core.MovieKey(1), Type: "movie", Title: "Film"}}, Total: 1}, nil
	}}
	no := false
	eight := 8
	probedMedia := core.MediaInfo{
		Container: "mpegts",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Profile: "High", Level: 40, Width: 1920, Height: 1080, BitDepth: 8, BitsPerRawSample: &eight, PixelFormat: "yuv420p", FrameRate: 29.97, BitrateKbps: 5000, ColorTransfer: "bt709", ColorSpace: "bt709", ColorPrimaries: "bt709", SampleAspectRatio: "1:1", DisplayAspectRatio: "16:9", Default: true, DolbyVisionConfigurationPresent: &no, HDR10Plus: &no},
			{Index: 1, Type: "audio", Codec: "aac", Profile: "LC", Channels: 2, SampleRate: 48000, BitrateKbps: 192, ChannelLayout: "stereo", Language: "eng", Default: true},
		},
	}
	sessions := frontendtest.Sessions{
		PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
			if r.Item != 0 || r.Channel != "abc" {
				t.Fatal(r.Item, r.Channel)
			}
			tuned = r.Channel
			return frontend.PlayPlan{ID: "plan", Decision: core.Decision{Kind: core.DecisionRemux, Container: "mpegts", AudioStream: -1, SubtitleStream: -1}, Media: core.MediaInfo{Container: "mpegts", Estimated: true}, Transcoding: true, HLS: true, HLSContainer: "mpegts"}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			m := probedMedia
			return frontend.Session{ID: "session", Media: &m}, nil
		},
		StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			return "/sessions/media/session/grant/live/index.m3u8", nil
		},
		StopFunc: func(_ context.Context, _ frontend.SessionAccess, id string) error {
			stoppedID = id
			return nil
		},
	}
	swiftfinBody, err := os.ReadFile("../../sessions/testdata/swiftfin-tvos-1.4.1-playbackinfo.json")
	if err != nil {
		t.Fatal(err)
	}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	for _, front := range []frontend.Frontend{Jellyfin{MachineID: "test"}, Emby{MachineID: "test"}} {
		libraryCalls = 0
		tuned = ""
		stoppedID = ""
		proto := string(front.Describe().ID)
		spec, specErr := openapi.CachedSpec("../../conform/specs/manifest.json", proto, "")
		var doc *openapi.Document
		if specErr == nil {
			doc, specErr = openapi.Parse(spec)
		}
		if specErr != nil && !os.IsNotExist(specErr) {
			t.Fatal(specErr)
		}
		inst, err := front.Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, LiveTV: live, Sessions: sessions, Capabilities: frontendtest.Capabilities{}})
		if err != nil {
			t.Fatal(err)
		}
		call := func(method, path, body string) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("X-Emby-Token", "grant")
			inst.Handler().ServeHTTP(w, r)
			if doc != nil && w.Code < 400 && (!strings.HasPrefix(path, "/Users/") || proto == "emby") && (path != "/LiveTv/Recordings" || proto != "emby") && !(method == "GET" && strings.HasPrefix(path, "/Items/") && proto == "emby") && !(path == "/Sessions/Playing/Stopped" && proto == "emby") {
				if err := doc.Validate(method, path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
					t.Fatalf("%s %s %s: %v\n%s", proto, method, path, err, w.Body.String())
				}
			}
			return w
		}
		list := call("GET", "/Items?IncludeItemTypes=LiveTvChannel&Recursive=true&SortBy=SortName&SortOrder=Ascending&Limit=1", "")
		if list.Code != 200 || libraryCalls != 0 || !strings.Contains(list.Body.String(), `"Name":"ABC10"`) || strings.Contains(list.Body.String(), "Virtual") || !strings.Contains(list.Body.String(), `"TotalRecordCount":2`) || !strings.Contains(list.Body.String(), `"Type":"TvChannel"`) || !strings.Contains(list.Body.String(), `"MediaSources":[`) {
			t.Fatal(proto, list.Code, libraryCalls, list.Body.String())
		}
		byIDs := call("GET", "/Items?Ids="+channelID+"&Fields=MediaSources,ParentId", "")
		if byIDs.Code != 200 || libraryCalls != 0 || !strings.Contains(byIDs.Body.String(), `"Name":"ABC10"`) || !strings.Contains(byIDs.Body.String(), `"OpenToken":"`+channelID+`"`) {
			t.Fatal(proto, byIDs.Code, libraryCalls, byIDs.Body.String())
		}
		movies := call("GET", "/Items?IncludeItemTypes=Movie&Recursive=true&Limit=5", "")
		if movies.Code != 200 || libraryCalls != 1 || !strings.Contains(movies.Body.String(), "Film") {
			t.Fatal(proto, movies.Code, libraryCalls, movies.Body.String())
		}
		recordings := call("GET", "/LiveTv/Recordings", "")
		if recordings.Code != 200 || !strings.Contains(recordings.Body.String(), `"Items":[]`) || !strings.Contains(recordings.Body.String(), `"TotalRecordCount":0`) {
			t.Fatal(proto, recordings.Code, recordings.Body.String())
		}
		for _, path := range []string{"/Items/" + progID, "/Users/" + userID + "/Items/" + progID, "/LiveTv/Programs/" + progID} {
			got := call("GET", path, "")
			if got.Code != 200 || !strings.Contains(got.Body.String(), `"Type":"Program"`) || !strings.Contains(got.Body.String(), `"Name":"ABC10 News"`) || !strings.Contains(got.Body.String(), `"ChannelId":"`+channelID+`"`) || !strings.Contains(got.Body.String(), `"StartDate":"2026-01-01T12:00:00.0000000Z"`) || strings.Contains(got.Body.String(), `"Items"`) {
				t.Fatal(proto, path, got.Code, got.Body.String())
			}
		}
		one := call("GET", "/Items/"+channelID, "")
		if one.Code != 200 || !strings.Contains(one.Body.String(), `"Type":"TvChannel"`) || !strings.Contains(one.Body.String(), `"Name":"ABC10"`) || !strings.Contains(one.Body.String(), `"OpenToken":"`+channelID+`"`) {
			t.Fatal(proto, one.Code, one.Body.String())
		}
		usersChannel := call("GET", "/Users/"+userID+"/Items/"+channelID, "")
		if usersChannel.Code != 200 || !strings.Contains(usersChannel.Body.String(), `"Type":"TvChannel"`) || !strings.Contains(usersChannel.Body.String(), `"MediaSources":[`) {
			t.Fatal(proto, usersChannel.Code, usersChannel.Body.String())
		}
		var reqMap map[string]any
		if err := json.Unmarshal(swiftfinBody, &reqMap); err != nil {
			t.Fatal(err)
		}
		reqMap["MediaSourceId"] = channelID
		reqBytes, _ := json.Marshal(reqMap)
		play := call("POST", "/Items/"+channelID+"/PlaybackInfo?UserId="+userID+"&StartTimeTicks=0&IsPlayback=true&AutoOpenLiveStream=true&MaxStreamingBitrate=128012886&MediaSourceId="+channelID, string(reqBytes))
		if play.Code != 200 || tuned != "abc" {
			t.Fatal(proto, play.Code, tuned, play.Body.String())
		}
		var pbResp struct {
			PlaySessionId string           `json:"PlaySessionId"`
			MediaSources  []map[string]any `json:"MediaSources"`
		}
		if err := json.Unmarshal(play.Body.Bytes(), &pbResp); err != nil || len(pbResp.MediaSources) != 1 {
			t.Fatal(proto, err, play.Body.String())
		}
		ms := pbResp.MediaSources[0]
		streams, _ := ms["MediaStreams"].([]any)
		if ms["Id"] != channelID || ms["OpenToken"] != channelID || ms["LiveStreamId"] != "session" || pbResp.PlaySessionId != "session" ||
			ms["Container"] != "ts" || ms["Protocol"] != "Http" || ms["IsRemote"] != false || ms["IsInfiniteStream"] != true ||
			ms["ReadAtNativeFramerate"] != true || ms["SupportsDirectPlay"] != false || ms["SupportsDirectStream"] != false ||
			ms["SupportsTranscoding"] != true || ms["SupportsProbing"] != true || ms["RequiresOpening"] != true || ms["RequiresClosing"] != true ||
			ms["TranscodingUrl"] != "/sessions/media/session/grant/live/index.m3u8" || ms["TranscodingSubProtocol"] != "hls" || ms["TranscodingContainer"] != "ts" ||
			len(streams) != 2 || ms["DefaultAudioStreamIndex"] != float64(1) {
			t.Fatalf("%s live MediaSource mismatch: %+v", proto, ms)
		}
		openResp := call("POST", "/LiveStreams/Open?OpenToken="+channelID, string(reqBytes))
		if openResp.Code != 200 || !strings.Contains(openResp.Body.String(), `"/sessions/media/session/grant/live/index.m3u8"`) {
			t.Fatal(proto, openResp.Code, openResp.Body.String())
		}
		closeResp := call("POST", "/LiveStreams/Close?liveStreamId=session", `{}`)
		wantCloseCode := 204
		if proto == "emby" {
			wantCloseCode = 200
		}
		if closeResp.Code != wantCloseCode || stoppedID != "session" {
			t.Fatal(proto, closeResp.Code, stoppedID, closeResp.Body.String())
		}
		stopped := call("POST", "/Sessions/Playing/Stopped", `{"ItemId":"`+channelID+`","MediaSourceId":"`+channelID+`","LiveStreamId":"session"}`)
		if stopped.Code != 204 {
			t.Fatal(proto, stopped.Code, stopped.Body.String())
		}
		playProg := call("POST", "/Items/"+progID+"/PlaybackInfo", `{"IsPlayback":true}`)
		if playProg.Code != 200 || tuned != "abc" || !strings.Contains(playProg.Body.String(), "/sessions/media/") {
			t.Fatal(proto, playProg.Code, tuned, playProg.Body.String())
		}
		if libraryCalls != 1 {
			t.Fatal(libraryCalls)
		}
		missing := call("GET", "/Items/"+virtualID, "")
		if missing.Code != 404 {
			t.Fatal(missing.Code, missing.Body.String())
		}
	}
}
