package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/transcode"
	"github.com/coder/websocket"
)

// Counting media used by the composed server. Opens are the actual engine
// opens; a second protocol playing the same release must not open again.
type operabilityMedia struct {
	mu     sync.Mutex
	opens  []string
	body   []byte
	tmdb   int64
	relID  string
	search int
}

func (m *operabilityMedia) Name() string { return "operability fixture" }
func (m *operabilityMedia) Search(_ context.Context, q core.SearchRequest) ([]core.Release, error) {
	m.mu.Lock()
	m.search++
	m.mu.Unlock()
	if q.IDs.TMDB != m.tmdb || q.Type != core.TypeMovie {
		return nil, nil
	}
	info := core.ReleaseInfo{Title: q.Title, Year: q.Year, Height: 720, Source: core.SourceWEBDL, VideoCodec: "h264", AudioCodec: "aac", Container: "mp4"}
	return []core.Release{{ID: m.relID, GUID: m.relID, Indexer: m.Name(), Name: q.Title + ".2000.720p.WEB-DL.H264.AAC", Size: 8 << 20, TitleIDs: q.IDs, Info: info}}, nil
}
func (m *operabilityMedia) FetchNZB(_ context.Context, r core.Release) ([]byte, error) {
	if r.ID != m.relID {
		return nil, core.ErrNotFound
	}
	return append([]byte(nil), m.body...), nil
}
func (m *operabilityMedia) Check(context.Context, []byte) (bool, error) { return true, nil }
func (m *operabilityMedia) CheckRelease(context.Context, string, []byte) (bool, error) {
	return true, nil
}
func (m *operabilityMedia) Open(_ context.Context, releaseID string, nzb []byte) (core.MediaSource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opens = append(m.opens, releaseID)
	if releaseID != m.relID || string(nzb) == "" {
		return nil, core.ErrNotFound
	}
	return &operabilitySource{id: releaseID, body: m.body}, nil
}
func (m *operabilityMedia) Prefetch(context.Context, string, []byte, int64, int64) error { return nil }
func (m *operabilityMedia) opensOf(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, got := range m.opens {
		if got == id {
			n++
		}
	}
	return n
}

type operabilitySource struct {
	id   string
	body []byte
}

func (s *operabilitySource) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(s.body)) {
		return 0, io.EOF
	}
	n := copy(p, s.body[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (s *operabilitySource) Size() int64       { return int64(len(s.body)) }
func (s *operabilitySource) Name() string      { return "movie.mp4" }
func (s *operabilitySource) ReleaseID() string { return s.id }
func (s *operabilitySource) Close() error      { return nil }

// One transcode budget for every protocol.
type operabilityTranscoder struct {
	mu    sync.Mutex
	live  map[string]core.HLSSession
	max   int
	start int
}

func (t *operabilityTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	return &core.MediaInfo{
		Container: "mp4", Duration: 90 * time.Minute, Size: 8 << 20, BitrateKbps: 4000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 720, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Default: true, Language: "eng"},
		},
	}, nil
}
func (t *operabilityTranscoder) Decide(info core.MediaInfo, _ core.ClientCaps, opts core.DecideOptions) core.Decision {
	if !opts.DirectPlay && !opts.DirectStream {
		return core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts"}
	}
	container := info.Container
	if container == "" {
		container = "mp4"
	}
	return core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: container}
}
func (t *operabilityTranscoder) StartHLS(_ context.Context, req core.HLSRequest) (core.HLSSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live == nil {
		t.live = map[string]core.HLSSession{}
	}
	if len(t.live) >= t.max {
		return nil, transcode.ErrTooManyTranscodes
	}
	id := req.SessionID
	if id == "" {
		id = "hls"
	}
	s := &operabilityHLS{id: id, lease: req.InputLease}
	t.live[id] = s
	t.start++
	return s, nil
}
func (t *operabilityTranscoder) Session(id string) (core.HLSSession, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.live[id]
	return s, ok
}
func (t *operabilityTranscoder) Stop(id string) {
	t.mu.Lock()
	s := t.live[id]
	delete(t.live, id)
	t.mu.Unlock()
	// A real encoder seals its input lease on stop. Leaving the lease held
	// blocks source retirement, so the owner session list never clears.
	if h, ok := s.(*operabilityHLS); ok && h.lease != nil {
		h.lease.SealInput()
	}
}
func (t *operabilityTranscoder) HWAccel() core.HWAccel { return core.HWAccel{Name: "none"} }
func (t *operabilityTranscoder) starts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.start
}

type operabilityHLS struct {
	id    string
	lease core.EncoderInputLease
}

func (s *operabilityHLS) ID() string { return s.id }
func (s *operabilityHLS) ServePlaylist(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "#EXTM3U\n")
}
func (s *operabilityHLS) ServeSegment(w http.ResponseWriter, _ *http.Request, n int) {
	_, _ = io.WriteString(w, "segment")
	_ = n
}
func (s *operabilityHLS) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error {
	return nil
}
func (s *operabilityHLS) LastAccess() time.Time { return time.Now() }
func (s *operabilityHLS) Close() error          { return nil }

func TestMixedProtocolOperability(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	media := &operabilityMedia{tmdb: 999, relID: "shared-release", body: make([]byte, 4096)}
	copy(media.body, []byte("not a media file"))
	budget := &operabilityTranscoder{max: 2}
	if err = cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Plex.Token = "test-owner"
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		c.Server.MaxStreams = 8
		c.Server.DefaultStreamLimit = 8
		c.Frontends = map[string]config.Frontend{
			"plex":     {Enabled: true},
			"jellyfin": {Enabled: true, Settings: map[string]any{"port": float64(18096), "discovery": false}},
			"emby":     {Enabled: true, Settings: map[string]any{"port": float64(18097), "discovery": false}},
			"silo":     {Enabled: true, Settings: map[string]any{"port": float64(18090), "discovery": false}},
			"stremio":  {Enabled: true},
			"webdav":   {Enabled: true},
			"m3u":      {Enabled: true},
			"dlna":     {Enabled: true, Settings: map[string]any{"discovery": false}},
		}
		for i := range c.Quality.Profiles {
			c.Quality.Profiles[i].MinSizeMBPerMin = 0
			c.Quality.Profiles[i].MaxSizeMBPerMin = 0
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	a, err := New(ctx, Options{
		DataDir:           dir,
		Logger:            slog.New(slog.NewTextHandler(&logs, nil)),
		FixtureMedia:      media,
		FixtureTranscoder: budget,
		plextvHTTP:        &http.Client{Transport: fakePlexTVTransport{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	if err = a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "test-owner"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	userGUID, err := ids.GUID(ids.User, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Popularity: 100, VoteCount: 10000, Key: core.MovieKey(media.tmdb), Type: core.TypeMovie, IDs: core.IDs{TMDB: media.tmdb}, Title: "Shared Release", Year: 2000, ContentRating: "PG-13", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	if err = a.frontends.control.PutSettings(ctx, "dlna", frontend.Settings{"viewerId": float64(owner.ID), "discovery": false}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	t.Cleanup(server.Close)
	a.frontends.start(ctx, server.Listener.Addr())
	t.Cleanup(a.frontends.stop)

	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[frontend.Protocol]string{}
	for _, p := range []frontend.Protocol{"jellyfin", "emby", "silo", "stremio", "webdav", "m3u"} {
		tokens[p], err = grants.Issue(ctx, owner.ID, nil, string(p), core.Device{ID: "device-" + string(p)})
		if err != nil {
			t.Fatal(err)
		}
	}
	client := server.Client()
	call := func(method, path, body string, header map[string]string) (int, []byte) {
		t.Helper()
		req, e := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		if body != "" && (strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[")) && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		data, e := io.ReadAll(res.Body)
		if e != nil {
			t.Fatal(e)
		}
		return res.StatusCode, data
	}
	ownerHeader := map[string]string{"X-Plex-Token": "test-owner", "X-Ether-Request": "1"}
	jf := map[string]string{"Authorization": `MediaBrowser Client="TV", Device="Living Room", DeviceId="device-jellyfin", Version="1", Token="` + tokens["jellyfin"] + `"`}
	em := map[string]string{"X-Emby-Token": tokens["emby"], "X-Emby-Device-Id": "device-emby"}
	silo := map[string]string{"Authorization": "Bearer " + tokens["silo"], "X-Silo-Device-Id": "device-silo", "X-Silo-Client": "tv"}
	guid := ids.RatingKeyGUID(title.Key)

	t.Run("simultaneous", func(t *testing.T) {
		type hit struct {
			path string
			want int
		}
		hits := []hit{
			{"/identity", 200},
			{"/jellyfin/System/Info/Public", 200},
			{"/emby/System/Info/Public", 200},
			{"/silo/api/v2/system/info", 200},
			{"/stremio/not-a-token/manifest.json", 401},
			{"/m3u/not-a-token/playlist.m3u", 401},
			{"/dlna/description.xml", 200},
		}
		var wg sync.WaitGroup
		errc := make(chan string, len(hits)+1)
		for _, h := range hits {
			wg.Add(1)
			go func(h hit) {
				defer wg.Done()
				code, body := call(http.MethodGet, h.path, "", nil)
				if code != h.want {
					errc <- h.path + " " + http.StatusText(code) + " " + string(body)
				}
			}(h)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := call("PROPFIND", "/webdav/", "", map[string]string{"Depth": "1"})
			if code != 401 {
				errc <- "webdav " + http.StatusText(code) + " " + string(body)
			}
		}()
		wg.Wait()
		close(errc)
		for msg := range errc {
			t.Error(msg)
		}
	})

	t.Run("status-health-logs", func(t *testing.T) {
		// A client sighting is written off the request path. The count stays
		// unknown until that write finishes, so poll briefly after the burst.
		var list []struct {
			Descriptor struct {
				ID string `json:"id"`
			} `json:"descriptor"`
			Enabled bool `json:"enabled"`
			Status  struct {
				Mounted          bool `json:"mounted"`
				LiveStreamsKnown bool `json:"liveStreamsKnown"`
				Clients24hKnown  bool `json:"clients24hKnown"`
			} `json:"status"`
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			code, body := call(http.MethodGet, "/ether/api/frontends", "", ownerHeader)
			if code != 200 {
				t.Fatal(code, string(body))
			}
			list = nil
			if err := json.Unmarshal(body, &list); err != nil {
				t.Fatal(err)
			}
			ready := len(list) > 0
			for _, item := range list {
				if !item.Enabled || !item.Status.Mounted || !item.Status.LiveStreamsKnown || !item.Status.Clients24hKnown {
					ready = false
					break
				}
			}
			if ready || !time.Now().Before(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		seen := map[string]bool{}
		for _, item := range list {
			seen[item.Descriptor.ID] = true
			if !item.Enabled || !item.Status.Mounted || !item.Status.LiveStreamsKnown || !item.Status.Clients24hKnown {
				t.Fatalf("status %+v", item)
			}
			hcode, hbody := call(http.MethodGet, "/ether/api/frontends/"+item.Descriptor.ID+"/health", "", ownerHeader)
			if hcode != 200 || !strings.Contains(string(hbody), `"healthy":true`) {
				t.Fatalf("health %s %d %s", item.Descriptor.ID, hcode, hbody)
			}
		}
		for _, id := range []string{"plex", "jellyfin", "emby", "silo", "stremio", "webdav", "m3u", "dlna"} {
			if !seen[id] {
				t.Fatalf("missing %s in %+v", id, seen)
			}
		}
		secret := "noisy-client-secret"
		for n := 0; n < 40; n++ {
			code, _ := call(http.MethodGet, "/jellyfin/System/Info?api_key="+secret, "", nil)
			if code != 401 {
				t.Fatal(code)
			}
		}
		lcode, lbody := call(http.MethodGet, "/ether/api/logs?q=authentication", "", ownerHeader)
		if lcode != 200 {
			t.Fatal(lcode, string(lbody))
		}
		if strings.Contains(string(lbody), secret) || strings.Contains(logs.String(), secret) {
			t.Fatal("failure log kept the client credential")
		}
		embySecret := "noisy-emby-secret"
		for n := 0; n < 40; n++ {
			code, _ := call(http.MethodGet, "/emby/System/Info?api_key="+embySecret, "", nil)
			if code != 401 {
				t.Fatal(code)
			}
		}
		if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), embySecret) {
			t.Fatal("failure log kept a client credential")
		}
		if strings.Count(logs.String(), `frontend=jellyfin`) != 1 || strings.Count(logs.String(), `frontend=emby`) != 1 {
			t.Fatal(logs.String())
		}
	})

	t.Run("browse-does-not-resolve", func(t *testing.T) {
		before := len(a.resolver.Traces())
		opens := media.opensOf(media.relID)
		soap := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ObjectID>0</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>20</RequestedCount><SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`
		type route struct {
			method, path, body string
			header             map[string]string
		}
		estimate := []route{
			{http.MethodGet, "/library/sections", "", ownerHeader},
			{http.MethodGet, "/library/sections/1/all", "", ownerHeader},
			{http.MethodGet, "/library/metadata/" + title.Key.String(), "", ownerHeader},
			{http.MethodGet, "/jellyfin/Users/" + userGUID + "/Views", "", jf},
			{http.MethodGet, "/jellyfin/Users/" + userGUID + "/Items", "", jf},
			{http.MethodPost, "/jellyfin/Items/" + guid + "/PlaybackInfo", `{"IsPlayback":false,"EnableDirectPlay":true}`, jf},
			{http.MethodGet, "/jellyfin/Items/" + guid, "", jf},
			{http.MethodGet, "/emby/Users/" + userGUID + "/Views", "", em},
			{http.MethodPost, "/emby/Items/" + guid + "/PlaybackInfo", `{"IsPlayback":false,"EnableDirectPlay":true}`, em},
			{http.MethodGet, "/emby/Users/" + userGUID + "/Items/" + guid, "", em},
			{http.MethodGet, "/silo/api/v2/catalog/items/" + guid, "", silo},
			{http.MethodGet, "/silo/api/v2/user/libraries", "", silo},
			{http.MethodGet, "/silo/api/v2/home/sections", "", silo},
			{http.MethodGet, "/stremio/" + tokens["stremio"] + "/catalog/movie/ether-movies.json", "", nil},
			{http.MethodGet, "/stremio/" + tokens["stremio"] + "/meta/movie/tmdb:999.json", "", nil},
			{http.MethodGet, "/stremio/" + tokens["stremio"] + "/stream/movie/tmdb:999.json", "", nil},
			{http.MethodGet, "/m3u/" + tokens["m3u"] + "/playlist.m3u", "", nil},
			{"PROPFIND", "/webdav/", "", map[string]string{"Depth": "1", "Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(owner.ID, 10)+":"+tokens["webdav"]))}},
			{http.MethodPost, "/dlna/ContentDirectory/control", soap, map[string]string{"SOAPACTION": `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`, "Content-Type": "text/xml"}},
		}
		check := func(r route) {
			t.Helper()
			code, body := call(r.method, r.path, r.body, r.header)
			if code >= 500 || code == 401 || code == 404 {
				t.Fatalf("%s %s -> %d %s", r.method, r.path, code, body)
			}
		}
		for _, r := range estimate {
			check(r)
		}
		if got := len(a.resolver.Traces()); got != before || media.opensOf(media.relID) != opens {
			t.Fatalf("browse resolved: traces %d -> %d opens %d -> %d", before, got, opens, media.opensOf(media.relID))
		}
	})

	t.Run("watch-state", func(t *testing.T) {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(wctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/emby/socket?api_key="+tokens["emby"], nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		if _, _, err = conn.Read(wctx); err != nil {
			t.Fatal(err)
		}
		code, body := call(http.MethodPost, "/jellyfin/Users/"+userGUID+"/PlayedItems/"+guid, "", jf)
		if code >= 300 {
			t.Fatalf("played %d %s", code, body)
		}
		_, msg, err := conn.Read(wctx)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(msg), "UserDataChanged") || !strings.Contains(string(msg), guid) {
			t.Fatal(string(msg))
		}
		code, body = call(http.MethodGet, "/emby/Users/"+userGUID+"/Items/"+guid, "", em)
		if code != 200 || !strings.Contains(string(body), `"Played":true`) {
			t.Fatalf("emby missed jellyfin watched state %d %s", code, body)
		}
		code, body = call(http.MethodGet, "/silo/api/v2/catalog/items/"+guid, "", silo)
		if code != 200 || !strings.Contains(string(body), `"played":true`) {
			t.Fatalf("silo missed jellyfin watched state %d %s", code, body)
		}
		code, body = call(http.MethodDelete, "/emby/Users/"+userGUID+"/PlayedItems/"+guid, "", em)
		if code >= 300 {
			t.Fatalf("emby unplayed %d %s", code, body)
		}
		code, body = call(http.MethodGet, "/jellyfin/Items/"+guid, "", jf)
		if code != 200 || strings.Contains(string(body), `"Played":true`) {
			t.Fatalf("jellyfin missed emby unplayed %d %s", code, body)
		}
		code, body = call(http.MethodGet, "/silo/api/v2/catalog/items/"+guid, "", silo)
		if code != 200 || strings.Contains(string(body), `"played":true`) {
			t.Fatalf("silo missed emby unplayed %d %s", code, body)
		}
	})

	t.Run("play-limits-cache-activity", func(t *testing.T) {
		for _, session := range a.playback.Sessions() {
			a.playback.Stop(session.ID)
		}
		opensBefore := media.opensOf(media.relID)
		plexHeader := map[string]string{"X-Plex-Token": "test-owner", "X-Plex-Client-Identifier": "plex-tv"}
		code, body := call(http.MethodGet, "/video/:/transcode/universal/start.m3u8?path="+urlPath(title.Key)+"&session=plex-direct&directPlay=1&directStream=1&protocol=hls", "", plexHeader)
		if code >= 500 {
			t.Fatalf("plex play %d %s\n%s", code, body, logs.String())
		}
		code, body = call(http.MethodPost, "/jellyfin/Items/"+guid+"/PlaybackInfo", `{"IsPlayback":true,"EnableDirectPlay":false,"EnableDirectStream":false,"EnableTranscoding":true}`, jf)
		if code != 200 {
			t.Fatalf("jellyfin transcode %d %s\n%s\ntraces %d search %d opens %v", code, body, logs.String(), len(a.resolver.Traces()), media.search, media.opens)
		}
		if budget.starts() != 2 {
			t.Fatalf("transcode starts %d", budget.starts())
		}
		code, body = call(http.MethodPost, "/emby/Items/"+guid+"/PlaybackInfo", `{"IsPlayback":true,"EnableDirectPlay":false,"EnableDirectStream":false,"EnableTranscoding":true}`, em)
		if code == 200 {
			t.Fatalf("emby started past the shared transcode budget: %s", body)
		}
		if budget.starts() != 2 {
			t.Fatalf("emby started another transcode: %d", budget.starts())
		}
		if got := media.opensOf(media.relID); got != opensBefore+1 {
			t.Fatalf("source opens = %d, want %d (%v)", got, opensBefore+1, media.opens)
		}
		pins := a.cache.Pins()
		if len(pins) != 1 || pins[media.relID] == nil {
			t.Fatalf("segment cache pins %+v", pins)
		}
		code, body = call(http.MethodGet, "/ether/api/status/sessions", "", ownerHeader)
		if code != 200 {
			t.Fatal(code, string(body))
		}
		var sessions []struct {
			ID     string `json:"ID"`
			Client struct {
				Protocol string `json:"Protocol"`
			} `json:"Client"`
		}
		var wrapped struct {
			Value []struct {
				ID     string `json:"ID"`
				Client struct {
					Protocol string `json:"Protocol"`
				} `json:"Client"`
			} `json:"value"`
		}
		if strings.Contains(string(body), `"value"`) {
			if err := json.Unmarshal(body, &wrapped); err != nil {
				t.Fatal(err, string(body))
			}
			for _, row := range wrapped.Value {
				sessions = append(sessions, struct {
					ID     string `json:"ID"`
					Client struct {
						Protocol string `json:"Protocol"`
					} `json:"Client"`
				}{ID: row.ID, Client: row.Client})
			}
		} else if err := json.Unmarshal(body, &sessions); err != nil {
			t.Fatal(err, string(body))
		}
		protocols := map[string]string{}
		for _, row := range sessions {
			if row.Client.Protocol != "" {
				protocols[row.Client.Protocol] = row.ID
			}
		}
		if protocols["plex"] == "" || protocols["jellyfin"] == "" {
			t.Fatalf("activity protocols %+v body %s", protocols, body)
		}
		for _, id := range []string{protocols["plex"], protocols["jellyfin"]} {
			code, body = call(http.MethodPost, "/ether/api/stats/sessions/"+id+"/stop", "{}", ownerHeader)
			if code != 200 {
				t.Fatalf("stop %s %d %s", id, code, body)
			}
		}
		code, body = call(http.MethodGet, "/ether/api/status/sessions", "", ownerHeader)
		if code != 200 || strings.Contains(string(body), protocols["plex"]) || strings.Contains(string(body), protocols["jellyfin"]) {
			t.Fatalf("sessions remain %d %s", code, body)
		}

		code, body = call(http.MethodPost, "/emby/Items/"+guid+"/PlaybackInfo", `{"IsPlayback":true,"EnableDirectPlay":true,"EnableDirectStream":true}`, em)
		if code != 200 {
			t.Fatalf("emby play %d %s", code, body)
		}
		var embyPlay struct {
			PlaySessionId string
		}
		if json.Unmarshal(body, &embyPlay) != nil || embyPlay.PlaySessionId == "" {
			t.Fatal(string(body))
		}
		// Ten minutes stays inside the 5%–90% resume window. One minute is below 5% and is not kept.
		code, body = call(http.MethodPost, "/emby/Sessions/Playing/Progress", `{"PlaySessionId":"`+embyPlay.PlaySessionId+`","PositionTicks":6000000000}`, em)
		if code != 204 {
			t.Fatalf("emby progress %d %s", code, body)
		}
		code, body = call(http.MethodGet, "/jellyfin/Users/"+userGUID+"/Items/"+guid, "", jf)
		if code != 200 || !strings.Contains(string(body), `"PlaybackPositionTicks":6000000000`) {
			t.Fatalf("jellyfin missed emby progress %d %s", code, body)
		}

		code, body = call(http.MethodGet, "/silo/api/v2/playback/capabilities", "", silo)
		var capsDoc struct {
			Installation string `json:"installation_id"`
		}
		if code != 200 || json.Unmarshal(body, &capsDoc) != nil || capsDoc.Installation == "" {
			t.Fatalf("silo capabilities %d %s", code, body)
		}
		code, body = call(http.MethodGet, "/silo/api/v2/catalog/items/"+guid, "", silo)
		var catalog struct {
			Versions []struct {
				FileID string `json:"file_id"`
			} `json:"versions"`
		}
		if code != 200 || json.Unmarshal(body, &catalog) != nil || len(catalog.Versions) == 0 || catalog.Versions[0].FileID == "" {
			t.Fatalf("silo file %d %s", code, body)
		}
		code, body = call(http.MethodPost, "/silo/api/v2/playback/start", `{"protocol_version":3,"file_id":"`+catalog.Versions[0].FileID+`","profile_id":"`+strconv.FormatInt(owner.ID, 10)+`","installation_id":"`+capsDoc.Installation+`","playback_attempt_id":"operability","client_capabilities":{"codecs_video":["h264"],"codecs_audio":["aac"],"containers":["mp4"]}}`, silo)
		if code >= 300 {
			t.Fatalf("silo play %d %s", code, body)
		}

		code, body = call(http.MethodGet, "/stremio/"+tokens["stremio"]+"/stream/movie/tmdb:999.json", "", nil)
		var streams struct {
			Streams []struct {
				URL string `json:"url"`
			} `json:"streams"`
		}
		if code != 200 || json.Unmarshal(body, &streams) != nil || len(streams.Streams) == 0 {
			t.Fatalf("stremio stream %d %s", code, body)
		}
		streamURL, err := url.Parse(streams.Streams[0].URL)
		if err != nil || streamURL.Path == "" {
			t.Fatal(streams.Streams[0].URL)
		}
		code, body = call(http.MethodGet, streamURL.RequestURI(), "", nil)
		if code >= 400 {
			t.Fatalf("stremio media %d %s", code, body)
		}

		movie := url.PathEscape("Shared Release (2000)")
		davAuth := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(owner.ID, 10)+":"+tokens["webdav"]))}
		code, body = call(http.MethodGet, "/webdav/Movies/"+movie+"/"+movie+".mkv", "", davAuth)
		if code >= 400 {
			t.Fatalf("webdav media %d %s", code, body)
		}

		dlnaKey := strconv.FormatInt(int64(title.Key), 10)
		dlna := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ObjectID>item:` + dlnaKey + `</ObjectID><BrowseFlag>BrowseMetadata</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>1</RequestedCount><SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`
		code, body = call(http.MethodPost, "/dlna/ContentDirectory/control", dlna, map[string]string{"SOAPACTION": `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`, "Content-Type": "text/xml"})
		if code != 200 {
			t.Fatalf("dlna item %d %s", code, body)
		}
		raw := string(body)
		marker := "/dlna/sessions/lazy/"
		start := strings.Index(raw, marker)
		if start < 0 {
			t.Fatal(raw)
		}
		end := strings.IndexAny(raw[start:], `<"&`)
		if end < 0 {
			t.Fatal(raw)
		}
		code, body = call(http.MethodGet, raw[start:start+end], "", nil)
		if code >= 400 {
			t.Fatalf("dlna media %d %s", code, body)
		}

		code, body = call(http.MethodGet, "/ether/api/status/sessions", "", ownerHeader)
		if code != 200 {
			t.Fatal(code, string(body))
		}
		listed := map[string]string{}
		var rows []struct {
			ID     string `json:"ID"`
			Client struct {
				Protocol string `json:"Protocol"`
			} `json:"Client"`
		}
		var wrappedRows struct {
			Value []struct {
				ID     string `json:"ID"`
				Client struct {
					Protocol string `json:"Protocol"`
				} `json:"Client"`
			} `json:"value"`
		}
		if strings.Contains(string(body), `"value"`) {
			if err = json.Unmarshal(body, &wrappedRows); err != nil {
				t.Fatal(err, string(body))
			}
			for _, row := range wrappedRows.Value {
				if row.Client.Protocol != "" {
					listed[row.Client.Protocol] = row.ID
				}
			}
		} else if err = json.Unmarshal(body, &rows); err != nil {
			t.Fatal(err, string(body))
		} else {
			for _, row := range rows {
				if row.Client.Protocol != "" {
					listed[row.Client.Protocol] = row.ID
				}
			}
		}
		for _, protocol := range []string{"emby", "silo", "stremio", "webdav", "dlna"} {
			id := listed[protocol]
			if id == "" {
				t.Fatalf("activity missing %s in %+v body %s", protocol, listed, body)
			}
			code, body = call(http.MethodPost, "/ether/api/stats/sessions/"+id+"/stop", "{}", ownerHeader)
			if code != 200 {
				t.Fatalf("stop %s %d %s", protocol, code, body)
			}
		}
		code, body = call(http.MethodGet, "/ether/api/status/sessions", "", ownerHeader)
		if code != 200 {
			t.Fatal(code, string(body))
		}
		for _, id := range listed {
			if strings.Contains(string(body), id) {
				t.Fatalf("session %s remains %s", id, body)
			}
		}
	})

	t.Run("hot-toggle-and-cluster", func(t *testing.T) {
		for _, id := range []string{"jellyfin", "emby", "silo", "stremio", "webdav", "m3u", "dlna", "plex"} {
			code, body := call(http.MethodPut, "/ether/api/frontends/"+id+"/enabled", `{"enabled":false}`, ownerHeader)
			if code != 200 {
				t.Fatalf("disable %s %d %s", id, code, body)
			}
			hcode, _ := call(http.MethodGet, "/ether/api/frontends/"+id+"/health", "", ownerHeader)
			if hcode != 503 {
				t.Fatalf("%s health after disable %d", id, hcode)
			}
			if _, mounted := a.frontends.registry.Instance(frontend.Protocol(id)); mounted {
				t.Fatalf("%s still mounted", id)
			}
			code, _ = call(http.MethodGet, "/ether/api/health", "", nil)
			if code != 200 {
				t.Fatal("owner listener died")
			}
			code, body = call(http.MethodPut, "/ether/api/frontends/"+id+"/enabled", `{"enabled":true}`, ownerHeader)
			if code != 200 {
				t.Fatalf("enable %s %d %s", id, code, body)
			}
			hcode, hbody := call(http.MethodGet, "/ether/api/frontends/"+id+"/health", "", ownerHeader)
			if hcode != 200 {
				t.Fatalf("%s health after enable %d %s", id, hcode, hbody)
			}
		}
		backend := &frontendSharedConfig{}
		if err := a.cfg.AttachShared(ctx, backend, config.SharedOptions{PollInterval: -1}); err != nil {
			t.Fatal(err)
		}
		dirB := t.TempDir()
		key, err := config.ReadCredentialKey(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err = config.InstallCredentialKey(dirB, key); err != nil {
			t.Fatal(err)
		}
		peer, err := config.Open(dirB)
		if err != nil {
			t.Fatal(err)
		}
		if err = peer.AttachShared(ctx, backend, config.SharedOptions{PollInterval: -1, NoSeed: true}); err != nil {
			t.Fatal(err)
		}
		frontends := []string{"jellyfin", "emby", "silo", "stremio", "webdav", "m3u", "dlna", "plex"}
		if err = peer.Update(func(c *config.Config) error {
			for _, id := range frontends {
				v := c.Frontends[id]
				v.Enabled = false
				c.Frontends[id] = v
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err = a.cfg.RefreshShared(ctx); err != nil {
			t.Fatal(err)
		}
		for _, id := range frontends {
			if _, mounted := a.frontends.registry.Instance(frontend.Protocol(id)); mounted {
				t.Fatalf("cluster disable left %s mounted", id)
			}
			hcode, _ := call(http.MethodGet, "/ether/api/frontends/"+id+"/health", "", ownerHeader)
			if hcode != 503 {
				t.Fatalf("cluster %s health %d", id, hcode)
			}
		}
		if err = peer.Update(func(c *config.Config) error {
			for _, id := range frontends {
				v := c.Frontends[id]
				v.Enabled = true
				c.Frontends[id] = v
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err = a.cfg.RefreshShared(ctx); err != nil {
			t.Fatal(err)
		}
		for _, id := range frontends {
			if _, mounted := a.frontends.registry.Instance(frontend.Protocol(id)); !mounted {
				t.Fatalf("cluster enable did not mount %s", id)
			}
		}
	})
}

func urlPath(key core.RatingKey) string {
	return "%2Flibrary%2Fmetadata%2F" + key.String()
}
