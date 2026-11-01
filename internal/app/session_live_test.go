package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLivePlaylistPathsUseScopedGrant(t *testing.T) {
	out := httptest.NewRecorder()
	w := &livePlaylistWriter{ResponseWriter: out, from: []byte("/livetv/sessions/internal/0/"), to: []byte("/sessions/media/id/grant/live/")}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte("#EXTM3U\n/livetv/sessions/internal/0/resource/"))
	_, _ = w.Write([]byte("abc\n"))
	w.finish()
	if got := out.Body.String(); !strings.Contains(got, "/sessions/media/id/grant/live/resource/abc") || strings.Contains(got, "/livetv/") {
		t.Fatalf("rewritten playlist %q", got)
	}
}

func TestLiveAdapterRewritesNestedResourcesToLazyCapability(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\n/variant?secret=source-token\n")
		case "/variant":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:4,\n/segment?secret=source-token\n")
		case "/segment":
			_, _ = io.WriteString(w, "media-bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	db := store.OpenTest(t)
	service, err := livetv.New(livetv.Options{DB: db, HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	channel, err := service.SaveChannel(context.Background(), livetv.Channel{Name: "Fixture", Number: "1", Kind: "iptv", Enabled: true, StreamURL: upstream.URL + "/master?secret=source-token"})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 1}
	info, err := service.Tune(context.Background(), viewer, channel.ID, core.ClientCaps{ClientID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := sessionLive{service: service}
	read := func(tail, base string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/?X-Plex-Token=account-token", nil)
		r.Header.Set("X-Plex-Token", "account-token")
		adapter.Serve(w, r, viewer, info.ID, tail, base)
		return w
	}
	firstURI := func(body, prefix string) string {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimPrefix(line, prefix)
			}
		}
		t.Fatalf("missing %q URI in %q", prefix, body)
		return ""
	}
	master := read("index.m3u8", "./")
	if master.Code != 200 || strings.Contains(master.Body.String(), "source-token") || strings.Contains(master.Body.String(), "account-token") || strings.Contains(master.Body.String(), "/sessions/media/") || !strings.Contains(master.Body.String(), "X-Ether-Stream-Token=") {
		t.Fatalf("master %d %q", master.Code, master.Body.String())
	}
	variantKey, _, _ := strings.Cut(firstURI(master.Body.String(), "./resource/"), "?")
	variant := read("resource/"+variantKey, "../")
	if variant.Code != 200 || strings.Contains(variant.Body.String(), "source-token") || strings.Contains(variant.Body.String(), "account-token") || !strings.Contains(variant.Body.String(), "X-Ether-Stream-Token=") {
		t.Fatalf("variant %d %q", variant.Code, variant.Body.String())
	}
	segmentKey, _, _ := strings.Cut(firstURI(variant.Body.String(), "../resource/"), "?")
	segment := read("resource/"+segmentKey, "../")
	if segment.Code != 200 || segment.Body.String() != "media-bytes" {
		t.Fatalf("segment %d %q", segment.Code, segment.Body.String())
	}
}

type liveTestHLS struct {
	id     string
	closed bool
}

func (h *liveTestHLS) ID() string            { return h.id }
func (h *liveTestHLS) LastAccess() time.Time { return time.Time{} }
func (h *liveTestHLS) Close() error          { h.closed = true; return nil }
func (h *liveTestHLS) ServePlaylist(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\n00000.ts\n")
}
func (h *liveTestHLS) ServeSegment(w http.ResponseWriter, _ *http.Request, n int) {
	w.Header().Set("Content-Type", "video/mp2t")
	_, _ = fmt.Fprintf(w, "virtual-ts-segment-%d", n)
}
func (h *liveTestHLS) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error {
	return nil
}

type liveTestTranscoder struct {
	probed     *core.MediaInfo
	probeDelay time.Duration
}

func (t *liveTestTranscoder) Probe(ctx context.Context, _ string) (*core.MediaInfo, error) {
	if t.probeDelay > 0 {
		select {
		case <-time.After(t.probeDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if t.probed != nil {
		cp := *t.probed
		return &cp, nil
	}
	return &core.MediaInfo{Container: "mpegts"}, nil
}
func (t *liveTestTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionRemux, Container: "mpegts", VideoCopy: true, AudioCopy: true, AudioStream: -1, SubtitleStream: -1}
}
func (t *liveTestTranscoder) StartHLS(_ context.Context, r core.HLSRequest) (core.HLSSession, error) {
	return &liveTestHLS{id: r.SessionID}, nil
}
func (t *liveTestTranscoder) Session(string) (core.HLSSession, bool) { return nil, false }
func (t *liveTestTranscoder) Stop(string)                            {}
func (t *liveTestTranscoder) HWAccel() core.HWAccel                  { return core.HWAccel{} }

type liveTestPlayback struct {
	core.Playback
	info     core.MediaInfo
	sessions []*core.PlaySession
}

func (p *liveTestPlayback) Prepare(_ context.Context, u *core.User, k core.RatingKey, c core.ClientCaps) (*core.PlaySession, error) {
	ps := &core.PlaySession{ID: fmt.Sprintf("virtual-ps-%d", len(p.sessions)+1), UserID: u.ID, Key: k, ClientID: c.ClientID, Info: p.info}
	p.sessions = append(p.sessions, ps)
	return ps, nil
}
func (p *liveTestPlayback) PrepareChannel(ctx context.Context, u *core.User, k core.RatingKey, c core.ClientCaps, g string) (*core.PlaySession, error) {
	ps, err := p.Prepare(ctx, u, k, c)
	if ps != nil {
		ps.PlaybackGroup = g
	}
	return ps, err
}
func (p *liveTestPlayback) InputURL(id string) string     { return "http://127.0.0.1/source/" + id }
func (p *liveTestPlayback) Stop(string)                   {}
func (p *liveTestPlayback) Sessions() []*core.PlaySession { return nil }
func (p *liveTestPlayback) Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo {
	return p.info
}
func (p *liveTestPlayback) PrepareSurface(ctx context.Context, u *core.User, k core.RatingKey, c core.ClientCaps, _ string, _ bool) (*core.PlaySession, error) {
	return p.Prepare(ctx, u, k, c)
}
func (p *liveTestPlayback) SessionForSurface(string, int64, string, core.RatingKey, string, bool) (*core.PlaySession, bool) {
	return nil, false
}
func (p *liveTestPlayback) MarkSurfaceOpened(string) bool                        { return true }
func (p *liveTestPlayback) MarkSurfacePrepared(string) bool                      { return true }
func (p *liveTestPlayback) Session(string) (*core.PlaySession, bool)             { return nil, false }
func (p *liveTestPlayback) SessionFor(int64, string, core.RatingKey) (*core.PlaySession, bool) {
	return nil, false
}
func (p *liveTestPlayback) ServeRange(http.ResponseWriter, *http.Request, string) {}
func (p *liveTestPlayback) Failover(context.Context, string, time.Duration) (*core.PlaySession, error) {
	return nil, core.ErrNotAvailable
}

type siloDeviceIdentity struct {
	frontendtest.Identity
}

func (i siloDeviceIdentity) ResolveDevice(ctx context.Context, credential string, _ frontend.Device) (*frontend.Viewer, time.Time, error) {
	v, err := i.Resolve(ctx, credential)
	if err != nil {
		return nil, time.Time{}, err
	}
	return v, time.Now().Add(time.Hour), nil
}

func TestSwiftfinLiveTVSequenceIPTVAndVirtual(t *testing.T) {
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iptv/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4500000,RESOLUTION=1920x1080\nvariant.m3u8?secret=upstream-token\n")
		case "/iptv/variant.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:4.0,\nseg0.ts?secret=upstream-token\n")
		case "/iptv/seg0.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = io.WriteString(w, "iptv-ts-segment-0")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	now := time.Unix(8, 0).UTC()
	iptvProbed := &core.MediaInfo{
		Container: "hls",
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Profile: "High", Level: 41, Width: 1920, Height: 1080, BitDepth: 8, PixelFormat: "yuv420p", FrameRate: 59.94, BitrateKbps: 4500, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Profile: "LC", Channels: 2, SampleRate: 48000, BitrateKbps: 128, ChannelLayout: "stereo", Language: "eng", Default: true},
		},
	}
	virtualProbed := core.MediaInfo{
		Container: "mkv",
		Duration:  20 * time.Second,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "hevc", Profile: "Main", Level: 120, Width: 1280, Height: 720, BitDepth: 8, PixelFormat: "yuv420p", FrameRate: 23.976, BitrateKbps: 2800, Default: true},
			{Index: 1, Type: "audio", Codec: "ac3", Channels: 6, SampleRate: 48000, BitrateKbps: 384, ChannelLayout: "5.1", Language: "eng", Default: true},
		},
	}
	pb := &liveTestPlayback{info: virtualProbed}
	tc := &liveTestTranscoder{probed: iptvProbed}
	db := store.OpenTest(t)
	watch := state.New(db, state.Options{})
	defer watch.Close(ctx)
	service, err := livetv.New(livetv.Options{DB: db, HTTPClient: upstream.Client(), Now: func() time.Time { return now }, Playback: pb, Transcoder: tc})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	iptvCh, err := service.SaveChannel(ctx, livetv.Channel{ID: "iptv-1", Name: "IPTV News", Number: "101", Kind: "iptv", Enabled: true, StreamURL: upstream.URL + "/iptv/master.m3u8?secret=upstream-token"})
	if err != nil {
		t.Fatal(err)
	}
	virtualCh, err := service.SaveChannel(ctx, livetv.Channel{ID: "virtual-1", Name: "Virtual Cinema", Number: "102", Kind: "virtual", Enabled: true, Epoch: time.Unix(0, 0).UTC(), Items: []livetv.Item{{Key: core.MovieKey(1), Title: "Feature Film", DurationMS: 20000, Rating: "PG"}}})
	if err != nil {
		t.Fatal(err)
	}

	viewer := &core.Viewer{ID: 1, Username: "viewer", IsOwner: true}
	userGUID, err := ids.GUID(ids.User, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeViewer := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id == viewer.ID {
			return viewer, nil
		}
		return nil, core.ErrNotFound
	}
	events := sessions.NewEventBus(nil, activeViewer)
	defer events.Close()
	broker, err := sessions.NewBroker(sessions.BrokerDeps{
		Events:       events,
		Playback:     pb,
		Transcoder:   tc,
		Live:         sessionLive{service: service, cached: func(_ context.Context, k core.RatingKey) (*core.Title, error) { return &core.Title{Key: k, Title: "Feature Film"}, nil }},
		Progress:     watch,
		ActiveViewer: activeViewer,
		CanPlay:      func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup:       func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) { return &core.Title{Key: k, Title: "Feature Film"}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)

	liveFacade := frontendtest.LiveTV{
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{
				{ID: iptvCh.ID, Name: iptvCh.Name, Number: iptvCh.Number, Kind: iptvCh.Kind},
				{ID: virtualCh.ID, Name: virtualCh.Name, Number: virtualCh.Number, Kind: virtualCh.Kind},
			}, nil
		},
		ResolveChannelFunc: func(_ context.Context, _ *frontend.Viewer, guid string) (frontend.LiveChannel, error) {
			for _, c := range []livetv.Channel{iptvCh, virtualCh} {
				id, _ := ids.StringGUID(ids.Channel, c.ID)
				if strings.EqualFold(id, guid) {
					return frontend.LiveChannel{ID: c.ID, Name: c.Name, Number: c.Number, Kind: c.Kind}, nil
				}
			}
			return frontend.LiveChannel{}, core.ErrNotFound
		},
	}
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, tok string) (*frontend.Viewer, error) {
		if tok == "grant" {
			return viewer, nil
		}
		return nil, core.ErrUnauthorized
	}}
	capBuilder := frontendtest.Capabilities{BuildFunc: func(ctx context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
		c, e := (caps.Builder{}).Build(ctx, req.Device, req.Profile, req.ProtocolInfo, req.UserAgent)
		return frontend.CapabilityResult{Client: c}, e
	}}

	swiftfinRaw, err := os.ReadFile("../sessions/testdata/swiftfin-tvos-1.4.1-playbackinfo.json")
	if err != nil {
		t.Fatal(err)
	}

	resolveRelative := func(basePath, rel string) string {
		u, err := url.Parse(rel)
		if err != nil {
			t.Fatal(err)
		}
		if path.IsAbs(u.Path) {
			return rel
		}
		joined := path.Join(path.Dir(basePath), u.Path)
		if u.RawQuery != "" {
			return joined + "?" + u.RawQuery
		}
		return joined
	}
	firstMediaURI := func(playlist string) string {
		for _, line := range strings.Split(playlist, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				return line
			}
		}
		t.Fatalf("no URI in playlist:\n%s", playlist)
		return ""
	}

	for _, front := range []frontend.Frontend{mediabrowser.Jellyfin{MachineID: "test"}, mediabrowser.Emby{MachineID: "test"}} {
		proto := front.Describe().ID
		inst, err := front.Mount(ctx, frontend.Host{
			Identity:     ident,
			LiveTV:       liveFacade,
			Sessions:     sessions.BindSurface(broker, proto),
			Capabilities: capBuilder,
		})
		if err != nil {
			t.Fatal(err)
		}
		request := func(method, target, body string, withHeader bool) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(method, target, strings.NewReader(body))
			if withHeader {
				r.Header.Set("X-Emby-Token", "grant")
				r.Header.Set("Authorization", `MediaBrowser Client="Swiftfin", Device="Apple TV", DeviceId="swiftfin-appletv", Version="1.4.1"`)
			}
			inst.Handler().ServeHTTP(w, r)
			return w
		}

		for _, tcChan := range []struct {
			ch        livetv.Channel
			wantCodec string
			wantAudio string
			wantWidth float64
			wantSeg   string
			hasMaster bool
		}{
			{ch: iptvCh, wantCodec: "h264", wantAudio: "aac", wantWidth: 1920, wantSeg: "iptv-ts-segment-0", hasMaster: true},
			{ch: virtualCh, wantCodec: "hevc", wantAudio: "ac3", wantWidth: 1280, wantSeg: "virtual-ts-segment-", hasMaster: false},
		} {
			channelGUID, err := ids.StringGUID(ids.Channel, tcChan.ch.ID)
			if err != nil {
				t.Fatal(err)
			}

			// Step 1a: Swiftfin Program.getChannel calls GET /Items?Ids={channelId}&Fields=MediaSources,ParentId
			byIDs := request("GET", "/Items?Ids="+channelGUID+"&Fields=MediaSources,ParentId", "", true)
			if byIDs.Code != 200 {
				t.Fatalf("%s %s GET /Items?Ids: %d %s", proto, tcChan.ch.Kind, byIDs.Code, byIDs.Body.String())
			}
			// Step 1b: Swiftfin Channel card calls GET /Items/{channelId}
			itemResp := request("GET", "/Items/"+channelGUID, "", true)
			if itemResp.Code != 200 {
				t.Fatalf("%s %s GET /Items/{id}: %d %s", proto, tcChan.ch.Kind, itemResp.Code, itemResp.Body.String())
			}
			var itemDTO struct {
				Id           string           `json:"Id"`
				MediaSources []map[string]any `json:"MediaSources"`
			}
			if err := json.Unmarshal(itemResp.Body.Bytes(), &itemDTO); err != nil || len(itemDTO.MediaSources) != 1 {
				t.Fatalf("%s %s item MediaSources: %v %s", proto, tcChan.ch.Kind, err, itemResp.Body.String())
			}
			initialMS := itemDTO.MediaSources[0]
			initialID, _ := initialMS["Id"].(string)
			if initialID != channelGUID || initialMS["OpenToken"] != channelGUID {
				t.Fatalf("%s %s initial MediaSource Id/OpenToken: %+v", proto, tcChan.ch.Kind, initialMS)
			}

			// Step 2: Swiftfin MediaPlayerItem.build calls POST /Items/{channelId}/PlaybackInfo?IsPlayback=true&AutoOpenLiveStream=true
			var pbReq map[string]any
			if err := json.Unmarshal(swiftfinRaw, &pbReq); err != nil {
				t.Fatal(err)
			}
			pbReq["UserId"] = userGUID
			pbReq["MediaSourceId"] = initialID
			pbReq["AutoOpenLiveStream"] = true
			pbReqBytes, _ := json.Marshal(pbReq)
			pbTarget := fmt.Sprintf("/Items/%s/PlaybackInfo?UserId=%s&StartTimeTicks=0&IsPlayback=true&AutoOpenLiveStream=true&MaxStreamingBitrate=128012886&MediaSourceId=%s", channelGUID, userGUID, initialID)
			pbW := request("POST", pbTarget, string(pbReqBytes), true)
			if pbW.Code != 200 {
				t.Fatalf("%s %s PlaybackInfo: %d %s", proto, tcChan.ch.Kind, pbW.Code, pbW.Body.String())
			}
			var pbDTO struct {
				PlaySessionId string           `json:"PlaySessionId"`
				MediaSources  []map[string]any `json:"MediaSources"`
			}
			if err := json.Unmarshal(pbW.Body.Bytes(), &pbDTO); err != nil || len(pbDTO.MediaSources) != 1 {
				t.Fatalf("%s %s PlaybackInfo decode: %v %s", proto, tcChan.ch.Kind, err, pbW.Body.String())
			}
			ms := pbDTO.MediaSources[0]
			openToken, _ := ms["OpenToken"].(string)
			liveStreamID, _ := ms["LiveStreamId"].(string)
			transcodingURL, _ := ms["TranscodingUrl"].(string)
			if !strings.Contains(openToken, initialID) || ms["Id"] != initialID || liveStreamID == "" || pbDTO.PlaySessionId != liveStreamID {
				t.Fatalf("%s %s MediaSource matching failed: %+v", proto, tcChan.ch.Kind, ms)
			}
			if ms["SupportsDirectPlay"] != false || ms["SupportsDirectStream"] != false || ms["SupportsTranscoding"] != true ||
				ms["IsInfiniteStream"] != true || ms["ReadAtNativeFramerate"] != true || ms["RequiresOpening"] != true || ms["RequiresClosing"] != true ||
				ms["Container"] != "ts" || ms["TranscodingContainer"] != "ts" || ms["TranscodingSubProtocol"] != "hls" || !strings.HasPrefix(transcodingURL, "/sessions/media/") {
				t.Fatalf("%s %s live flags/container mismatch: %+v", proto, tcChan.ch.Kind, ms)
			}
			streams, _ := ms["MediaStreams"].([]any)
			if len(streams) != 2 {
				t.Fatalf("%s %s expected 2 MediaStreams, got %+v", proto, tcChan.ch.Kind, streams)
			}
			vStream, _ := streams[0].(map[string]any)
			aStream, _ := streams[1].(map[string]any)
			if vStream["Codec"] != tcChan.wantCodec || vStream["Width"] != tcChan.wantWidth || aStream["Codec"] != tcChan.wantAudio {
				t.Fatalf("%s %s probed streams mismatch: video=%+v audio=%+v", proto, tcChan.ch.Kind, vStream, aStream)
			}

			// Step 3: Swiftfin AVPlayer fetches TranscodingUrl with ?token=grant&api_key=grant (without X-Emby-Token header)
			swiftfinHLSURL := transcodingURL + "?token=grant&api_key=grant"
			plW := request("GET", swiftfinHLSURL, "", false)
			if plW.Code != 200 || !strings.Contains(plW.Body.String(), "#EXTM3U") || strings.Contains(plW.Body.String(), "upstream-token") {
				t.Fatalf("%s %s playlist fetch (%s): %d %s", proto, tcChan.ch.Kind, swiftfinHLSURL, plW.Code, plW.Body.String())
			}

			// Step 4: Follow variant playlist (IPTV) and fetch a .ts segment
			nextURI := resolveRelative(transcodingURL, firstMediaURI(plW.Body.String()))
			if tcChan.hasMaster {
				varW := request("GET", nextURI, "", false)
				if varW.Code != 200 || !strings.Contains(varW.Body.String(), "#EXTINF") || strings.Contains(varW.Body.String(), "upstream-token") {
					t.Fatalf("%s %s variant fetch (%s): %d %s", proto, tcChan.ch.Kind, nextURI, varW.Code, varW.Body.String())
				}
				varPath, _, _ := strings.Cut(nextURI, "?")
				nextURI = resolveRelative(varPath, firstMediaURI(varW.Body.String()))
			}
			segW := request("GET", nextURI, "", false)
			if segW.Code != 200 || !strings.HasPrefix(segW.Body.String(), tcChan.wantSeg) {
				t.Fatalf("%s %s segment fetch (%s): %d %q", proto, tcChan.ch.Kind, nextURI, segW.Code, segW.Body.String())
			}

			// Step 5: Swiftfin reports Playing / Progress, then closes LiveStream and reports Stopped
			playingBody := fmt.Sprintf(`{"ItemId":%q,"MediaSourceId":%q,"PlaySessionId":%q,"LiveStreamId":%q,"PositionTicks":0}`, channelGUID, initialID, pbDTO.PlaySessionId, liveStreamID)
			if w := request("POST", "/Sessions/Playing", playingBody, true); w.Code != 204 {
				t.Fatalf("%s %s Playing: %d %s", proto, tcChan.ch.Kind, w.Code, w.Body.String())
			}
			if w := request("POST", "/Sessions/Playing/Progress", playingBody, true); w.Code != 204 {
				t.Fatalf("%s %s Progress: %d %s", proto, tcChan.ch.Kind, w.Code, w.Body.String())
			}
			closeW := request("POST", "/LiveStreams/Close?liveStreamId="+liveStreamID, `{}`, true)
			wantClose := 204
			if proto == "emby" {
				wantClose = 200
			}
			if closeW.Code != wantClose {
				t.Fatalf("%s %s LiveStreams/Close: %d %s", proto, tcChan.ch.Kind, closeW.Code, closeW.Body.String())
			}
			if w := request("POST", "/Sessions/Playing/Stopped", playingBody, true); w.Code != 204 {
				t.Fatalf("%s %s Stopped after Close: %d %s", proto, tcChan.ch.Kind, w.Code, w.Body.String())
			}
		}
	}
}

// siloScopeAllows replicates Silo Apple TV's PlaybackMediaAuthorization.Scope.allows(url)
// contract so live playlist and segment URLs are verified against the exact client rules.
func siloScopeAllows(sessionID, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.RawQuery != "" {
		return fmt.Errorf("Silo Scope forbids query string on live media URI: %q", rawURL)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) == 5 && parts[0] == "api" && parts[1] == "v2" && parts[2] == "playback" && parts[3] == "transcode" && parts[4] == sessionID {
		return fmt.Errorf("incomplete transcode path %q", rawURL)
	}
	if len(parts) == 6 && parts[0] == "api" && parts[1] == "v2" && parts[2] == "playback" && parts[3] == "transcode" && parts[4] == sessionID && parts[5] == "master.m3u8" {
		return nil
	}
	if len(parts) != 7 || parts[0] != "api" || parts[1] != "v2" || parts[2] != "playback" || parts[3] != "transcode" || parts[4] != sessionID || parts[5] != "segment" {
		return fmt.Errorf("URI %q outside Silo /api/v2/playback/transcode/%s/segment/<name> scope", rawURL, sessionID)
	}
	seg := parts[6]
	if seg == "" || seg == "." || seg == ".." {
		return fmt.Errorf("invalid segment component %q", seg)
	}
	for _, r := range seg {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '~') {
			return fmt.Errorf("unsafe character %q in segment component %q", r, seg)
		}
	}
	return nil
}

func TestSiloLiveTVSequenceIPTVVirtualAndReplan(t *testing.T) {
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iptv/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4500000,RESOLUTION=1920x1080\nvariant.m3u8?secret=upstream-token\n")
		case "/iptv/variant.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:4.0,\nseg0.ts?secret=upstream-token\n")
		case "/iptv/seg0.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = io.WriteString(w, "iptv-ts-segment-0")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	now := time.Unix(8, 0).UTC()
	virtualProbed := core.MediaInfo{
		Container: "mkv",
		Duration:  20 * time.Second,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "hevc", Profile: "Main", Level: 120, Width: 1280, Height: 720, BitDepth: 8, PixelFormat: "yuv420p", FrameRate: 23.976, BitrateKbps: 2800, Default: true},
			{Index: 1, Type: "audio", Codec: "ac3", Channels: 6, SampleRate: 48000, BitrateKbps: 384, ChannelLayout: "5.1", Language: "eng", Default: true},
		},
	}
	pb := &liveTestPlayback{info: virtualProbed}
	// Simulate a slow 1.5s live ffprobe to verify Silo playback/start still completes under 1s.
	tc := &liveTestTranscoder{probeDelay: 1500 * time.Millisecond}
	db := store.OpenTest(t)
	watch := state.New(db, state.Options{})
	defer watch.Close(ctx)
	service, err := livetv.New(livetv.Options{DB: db, HTTPClient: upstream.Client(), Now: func() time.Time { return now }, Playback: pb, Transcoder: tc})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	iptvCh, err := service.SaveChannel(ctx, livetv.Channel{ID: "iptv-bloomberg", Name: "Bloomberg TV+", Number: "5", Kind: "iptv", Enabled: true, StreamURL: upstream.URL + "/iptv/master.m3u8?secret=upstream-token"})
	if err != nil {
		t.Fatal(err)
	}
	virtualCh, err := service.SaveChannel(ctx, livetv.Channel{ID: "virtual-atv", Name: "ATV Test Cached", Number: "3", Kind: "virtual", Enabled: true, Epoch: time.Unix(0, 0).UTC(), Items: []livetv.Item{{Key: core.MovieKey(1), Title: "Cached Programme", DurationMS: 20000, Rating: "PG"}}})
	if err != nil {
		t.Fatal(err)
	}

	viewer := &core.Viewer{ID: 1, Username: "viewer", IsOwner: true}
	activeViewer := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id == viewer.ID {
			return viewer, nil
		}
		return nil, core.ErrNotFound
	}
	events := sessions.NewEventBus(nil, activeViewer)
	defer events.Close()
	broker, err := sessions.NewBroker(sessions.BrokerDeps{
		Events:       events,
		Playback:     pb,
		Transcoder:   tc,
		Live:         sessionLive{service: service, cached: func(_ context.Context, k core.RatingKey) (*core.Title, error) { return &core.Title{Key: k, Title: "Cached Programme"}, nil }},
		Progress:     watch,
		ActiveViewer: activeViewer,
		CanPlay:      func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup:       func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) { return &core.Title{Key: k, Title: "Cached Programme"}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)

	liveFacade := frontendtest.LiveTV{
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{
				{ID: iptvCh.ID, Name: iptvCh.Name, Number: iptvCh.Number, Kind: iptvCh.Kind},
				{ID: virtualCh.ID, Name: virtualCh.Name, Number: virtualCh.Number, Kind: virtualCh.Kind},
			}, nil
		},
		ResolveChannelFunc: func(_ context.Context, _ *frontend.Viewer, guid string) (frontend.LiveChannel, error) {
			for _, c := range []livetv.Channel{iptvCh, virtualCh} {
				id, _ := ids.StringGUID(ids.Channel, c.ID)
				if strings.EqualFold(id, guid) {
					return frontend.LiveChannel{ID: c.ID, Name: c.Name, Number: c.Number, Kind: c.Kind}, nil
				}
			}
			return frontend.LiveChannel{}, core.ErrNotFound
		},
	}
	ident := siloDeviceIdentity{frontendtest.Identity{ResolveFunc: func(_ context.Context, tok string) (*frontend.Viewer, error) {
		if tok == "grant" {
			return viewer, nil
		}
		return nil, core.ErrUnauthorized
	}}}
	capBuilder := frontendtest.Capabilities{BuildFunc: func(ctx context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
		c, e := (caps.Builder{}).Build(ctx, req.Device, req.Profile, req.ProtocolInfo, req.UserAgent)
		return frontend.CapabilityResult{Client: c}, e
	}}

	inst, err := (silo.Frontend{MachineID: "silo-live-test"}).Mount(ctx, frontend.Host{
		Identity:     ident,
		LiveTV:       liveFacade,
		Sessions:     sessions.BindSurface(broker, "silo"),
		Capabilities: capBuilder,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, target, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "apple-tv-1")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		inst.Handler().ServeHTTP(w, r)
		return w
	}

	capW := request("GET", "/api/v2/playback/capabilities", "")
	var capDoc struct {
		InstallationID string `json:"installation_id"`
	}
	if capW.Code != 200 || json.Unmarshal(capW.Body.Bytes(), &capDoc) != nil || capDoc.InstallationID == "" {
		t.Fatalf("capabilities: %d %s", capW.Code, capW.Body.String())
	}

	firstMediaURI := func(playlist string) string {
		for _, line := range strings.Split(playlist, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				return line
			}
		}
		t.Fatalf("no URI in playlist:\n%s", playlist)
		return ""
	}

	for _, tcChan := range []struct {
		ch        livetv.Channel
		wantSeg   string
		hasMaster bool
	}{
		{ch: iptvCh, wantSeg: "iptv-ts-segment-0", hasMaster: true},
		{ch: virtualCh, wantSeg: "virtual-ts-segment-", hasMaster: false},
	} {
		channelGUID, err := ids.StringGUID(ids.Channel, tcChan.ch.ID)
		if err != nil {
			t.Fatal(err)
		}
		watchW := request("GET", "/api/v2/watch/"+channelGUID, "")
		if watchW.Code != 200 {
			t.Fatalf("%s watch: %d %s", tcChan.ch.Kind, watchW.Code, watchW.Body.String())
		}
		var watchDoc struct {
			Versions []struct {
				FileID string `json:"file_id"`
			} `json:"versions"`
		}
		if err := json.Unmarshal(watchW.Body.Bytes(), &watchDoc); err != nil || len(watchDoc.Versions) != 1 || watchDoc.Versions[0].FileID == "" {
			t.Fatalf("%s watch decode: %v %s", tcChan.ch.Kind, err, watchW.Body.String())
		}
		fileID := watchDoc.Versions[0].FileID

		startAt := time.Now()
		startBody := fmt.Sprintf(`{"protocol_version":3,"file_id":%q,"profile_id":"1","installation_id":%q,"playback_attempt_id":"attempt-%s","quality_preference":"auto","start_position":0,"client_capabilities":{"codecs_video":["h264","hevc"],"codecs_audio":["aac","ac3"],"containers":["mp4","mov"],"hdr":true,"max_resolution":"2160p"}}`, fileID, capDoc.InstallationID, tcChan.ch.Kind)
		startW := request("POST", "/api/v2/playback/start", startBody)
		elapsed := time.Since(startAt)
		if startW.Code != 201 {
			t.Fatalf("%s start: %d %s", tcChan.ch.Kind, startW.Code, startW.Body.String())
		}
		if elapsed >= time.Second {
			t.Fatalf("%s playback/start took %v (must be < 1s)", tcChan.ch.Kind, elapsed)
		}
		var startDoc struct {
			Outcome      string `json:"outcome"`
			SessionID    string `json:"session_id"`
			PlaybackPlan struct {
				PlanID          string `json:"plan_id"`
				PlanAttemptKey  string `json:"plan_attempt_key"`
				Delivery        string `json:"delivery"`
				EffectiveRecipe struct {
					VideoCodec string `json:"video_codec"`
					AudioCodec string `json:"audio_codec"`
				} `json:"effective_recipe"`
				SelectedTracks struct {
					Audio map[string]any `json:"audio"`
				} `json:"selected_tracks"`
				Stream struct {
					URL string `json:"url"`
				} `json:"stream"`
			} `json:"playback_plan"`
		}
		if err := json.Unmarshal(startW.Body.Bytes(), &startDoc); err != nil {
			t.Fatal(err)
		}
		if startDoc.Outcome != "playable" || startDoc.SessionID == "" || startDoc.PlaybackPlan.PlanAttemptKey == "" ||
			startDoc.PlaybackPlan.EffectiveRecipe.VideoCodec == "" || startDoc.PlaybackPlan.EffectiveRecipe.AudioCodec == "" ||
			startDoc.PlaybackPlan.SelectedTracks.Audio == nil {
			t.Fatalf("%s incomplete live plan: %s", tcChan.ch.Kind, startW.Body.String())
		}
		if err := siloScopeAllows(startDoc.SessionID, startDoc.PlaybackPlan.Stream.URL); err != nil {
			t.Fatalf("%s master URL rejected by Silo scope: %v", tcChan.ch.Kind, err)
		}

		// Open master.m3u8
		masterW := request("GET", startDoc.PlaybackPlan.Stream.URL, "")
		if masterW.Code != 200 || !strings.Contains(masterW.Body.String(), "#EXTM3U") || strings.Contains(masterW.Body.String(), "upstream-token") {
			t.Fatalf("%s master: %d %s", tcChan.ch.Kind, masterW.Code, masterW.Body.String())
		}
		nextURI := firstMediaURI(masterW.Body.String())
		if err := siloScopeAllows(startDoc.SessionID, nextURI); err != nil {
			t.Fatalf("%s playlist URI rejected by Silo scope: %v (playlist:\n%s)", tcChan.ch.Kind, err, masterW.Body.String())
		}
		if tcChan.hasMaster {
			varW := request("GET", nextURI, "")
			if varW.Code != 200 || !strings.Contains(varW.Body.String(), "#EXTINF") || strings.Contains(varW.Body.String(), "upstream-token") {
				t.Fatalf("%s variant: %d %s", tcChan.ch.Kind, varW.Code, varW.Body.String())
			}
			nextURI = firstMediaURI(varW.Body.String())
			if err := siloScopeAllows(startDoc.SessionID, nextURI); err != nil {
				t.Fatalf("%s segment URI rejected by Silo scope: %v (variant:\n%s)", tcChan.ch.Kind, err, varW.Body.String())
			}
		}
		segW := request("GET", nextURI, "")
		if segW.Code != 200 || !strings.HasPrefix(segW.Body.String(), tcChan.wantSeg) {
			t.Fatalf("%s first segment (%s): %d %q", tcChan.ch.Kind, nextURI, segW.Code, segW.Body.String())
		}

		// Failed route -> replan must return a different plan_attempt_key/route, and then adaptation_unavailable once exhausted.
		replan1Body := fmt.Sprintf(`{"installation_id":%q,"protocol_version":3,"operation":"failure_recovery","playback_attempt_id":"attempt-%s","replan_request_id":"replan-1","failed_plan_id":%q,"plan_attempt_id":"pa-1","plan_attempt_key":%q,"attempted_plan_keys":[%q],"attempt_count":1,"quality_preference":"auto","position_seconds":0}`,
			capDoc.InstallationID, tcChan.ch.Kind, startDoc.PlaybackPlan.PlanID, startDoc.PlaybackPlan.PlanAttemptKey, startDoc.PlaybackPlan.PlanAttemptKey)
		replan1W := request("POST", "/api/v2/playback/"+startDoc.SessionID+"/replan", replan1Body)
		if replan1W.Code != 200 {
			t.Fatalf("%s replan 1: %d %s", tcChan.ch.Kind, replan1W.Code, replan1W.Body.String())
		}
		var replan1Doc struct {
			Outcome      string `json:"outcome"`
			PlaybackPlan struct {
				PlanID         string `json:"plan_id"`
				PlanAttemptKey string `json:"plan_attempt_key"`
				Delivery       string `json:"delivery"`
			} `json:"playback_plan"`
		}
		if err := json.Unmarshal(replan1W.Body.Bytes(), &replan1Doc); err != nil {
			t.Fatal(err)
		}
		if replan1Doc.Outcome != "playable" || replan1Doc.PlaybackPlan.PlanAttemptKey == "" || replan1Doc.PlaybackPlan.PlanAttemptKey == startDoc.PlaybackPlan.PlanAttemptKey || replan1Doc.PlaybackPlan.Delivery == startDoc.PlaybackPlan.Delivery {
			t.Fatalf("%s replan 1 returned duplicate plan or route: %s", tcChan.ch.Kind, replan1W.Body.String())
		}

		replan2Body := fmt.Sprintf(`{"installation_id":%q,"protocol_version":3,"operation":"failure_recovery","playback_attempt_id":"attempt-%s","replan_request_id":"replan-2","failed_plan_id":%q,"plan_attempt_id":"pa-2","plan_attempt_key":%q,"attempted_plan_keys":[%q,%q],"attempt_count":2,"quality_preference":"auto","position_seconds":0}`,
			capDoc.InstallationID, tcChan.ch.Kind, replan1Doc.PlaybackPlan.PlanID, replan1Doc.PlaybackPlan.PlanAttemptKey, startDoc.PlaybackPlan.PlanAttemptKey, replan1Doc.PlaybackPlan.PlanAttemptKey)
		replan2W := request("POST", "/api/v2/playback/"+startDoc.SessionID+"/replan", replan2Body)
		if replan2W.Code != 200 {
			t.Fatalf("%s replan 2: %d %s", tcChan.ch.Kind, replan2W.Code, replan2W.Body.String())
		}
		var replan2Doc struct {
			Outcome  string `json:"outcome"`
			Terminal struct {
				Reason string `json:"reason"`
			} `json:"terminal"`
		}
		if err := json.Unmarshal(replan2W.Body.Bytes(), &replan2Doc); err != nil {
			t.Fatal(err)
		}
		if replan2Doc.Outcome != "adaptation_unavailable" || replan2Doc.Terminal.Reason == "" {
			t.Fatalf("%s replan 2 expected adaptation_unavailable, got: %s", tcChan.ch.Kind, replan2W.Body.String())
		}
	}
}
