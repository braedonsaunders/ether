package livetv

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func testService(t *testing.T, o Options) *Service {
	t.Helper()
	if o.DB == nil {
		db, e := store.OpenMemory(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		o.DB = db
	}
	s, e := New(o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func testChannel() Channel {
	return Channel{ID: "test", Number: "1", Name: "Owner channel", Kind: "virtual", Enabled: true, Epoch: time.Unix(0, 0).UTC(), Items: []Item{{Key: core.MovieKey(1), Title: "First", DurationMS: 12000, Rating: "G"}, {Key: core.MovieKey(2), Title: "Second", DurationMS: 20000, Rating: "PG"}}}
}
func TestScheduleWindowIndependentAndOffset(t *testing.T) {
	c := testChannel()
	from := time.Unix(103, 0)
	all := Schedule(c, from, from.Add(2*time.Minute))
	if len(all) == 0 {
		t.Fatal("no schedule")
	}
	narrow := Schedule(c, from.Add(3*time.Second), from.Add(4*time.Second))
	if len(narrow) != 1 || !reflect.DeepEqual(narrow[0], all[0]) {
		t.Fatalf("window changed programme: %#v / %#v", all, narrow)
	}
	for i, p := range all {
		if i > 0 && !p.Start.Equal(all[i-1].End) {
			t.Fatal("gap or overlap")
		}
		if p.Offset(p.Start.Add(3*time.Second)) != 3*time.Second || p.Offset(p.End.Add(time.Second)) != p.End.Sub(p.Start) {
			t.Fatal("bad join offset")
		}
	}
	c.Shuffle = true
	c.Seed = 71
	again := Schedule(c, from, from.Add(time.Minute))
	restart := Schedule(c, from, from.Add(time.Minute))
	if !reflect.DeepEqual(again, restart) {
		t.Fatal("shuffle changes on restart")
	}
	for _, p := range again {
		single := Schedule(c, p.Start.Add(time.Second), p.Start.Add(2*time.Second))
		if len(single) != 1 || !reflect.DeepEqual(single[0], p) {
			t.Fatal("shuffle depends on query window")
		}
	}
}
func TestDailyBlocksClipAndFill(t *testing.T) {
	c := testChannel()
	c.Items = []Item{{Key: core.MovieKey(1), Title: "Base", DurationMS: 70 * 60000}}
	c.Blocks = []Block{{Start: 60, End: 120, Items: []Item{{Key: core.MovieKey(2), Title: "Morning", DurationMS: 45 * 60000}}}}
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	ps := Schedule(c, day, day.Add(4*time.Hour))
	if len(ps) < 5 {
		t.Fatalf("short schedule: %+v", ps)
	}
	for i, p := range ps {
		if i > 0 && !p.Start.Equal(ps[i-1].End) {
			t.Fatal("time block leaves a gap")
		}
		if p.Start.Equal(day.Add(time.Hour)) && p.Item.Title != "Morning" {
			t.Fatal("block did not start on time")
		}
		if p.Start.Equal(day.Add(105*time.Minute)) && !p.End.Equal(day.Add(2*time.Hour)) {
			t.Fatal("last block programme not clipped")
		}
	}
	c.Blocks = append(c.Blocks, Block{Start: 90, End: 150, Items: c.Items})
	if validateChannel(&c) == nil {
		t.Fatal("overlapping blocks accepted")
	}
}
func TestM3UAndXMLTVFixtures(t *testing.T) {
	m3u := "\ufeff#EXTM3U\n#EXTINF:-1 tvg-id=\"one\" tvg-name=\"Name, with comma\" tvg-logo=\"https://owner.invalid/logo.png\" tvg-chno=\"3.2\" group-title=\"Owner\",Channel & One\nhttps://owner.invalid/live/master.m3u8?account=owner\n"
	cs, e := ParseM3U(strings.NewReader(m3u), "source")
	if e != nil || len(cs) != 1 {
		t.Fatalf("parse: %v %+v", e, cs)
	}
	if cs[0].Name != "Channel & One" || cs[0].Number != "3.2" || cs[0].GuideID != "one" {
		t.Fatalf("wrong metadata: %+v", cs[0])
	}
	cs2, _ := ParseM3U(strings.NewReader(m3u), "source")
	if cs2[0].ID != cs[0].ID {
		t.Fatal("unstable imported id")
	}
	bad := []string{"not a playlist", "#EXTM3U\n#EXTINF:-1,Unsafe\nfile:///private/secret", "#EXTM3U\n#EXTINF:-1,Dangling"}
	for _, v := range bad {
		if _, e := ParseM3U(strings.NewReader(v), "s"); e == nil {
			t.Fatalf("accepted %q", v)
		}
	}
	guide := `<?xml version="1.0"?><tv><channel id="one"><display-name>Channel &amp; One</display-name></channel><programme channel="one" start="20261019090000 -0400" stop="20261019100000 -0400"><title>Morning &amp; News</title><desc>Owner guide</desc><rating system="MPAA"><value>PG</value></rating><icon src="https://owner.invalid/cover.png"/></programme></tv>`
	g, e := ParseXMLTV(strings.NewReader(guide))
	if e != nil {
		t.Fatal(e)
	}
	p := g.Programmes[0]
	if p.Start.Hour() != 13 || p.End.Sub(p.Start) != time.Hour || p.Item.Rating != "PG" || p.Item.Title != "Morning & News" {
		t.Fatalf("XMLTV lost data: %+v", p)
	}
	if _, e := ParseXMLTV(strings.NewReader(`<tv><programme channel="one" start="bad" stop="bad"/></tv>`)); e == nil {
		t.Fatal("invalid timestamps accepted")
	}
}
func TestPersistenceImportsPoliciesAndAtomicFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 13, 30, 0, 0, time.UTC)
	s := testService(t, Options{Now: func() time.Time { return now }})
	src := Source{ID: "owner-source", Name: "Owner source", Playlist: "#EXTM3U\n#EXTINF:-1 tvg-id=\"one\",One\nhttps://owner.invalid/master.m3u8\n", Guide: `<tv><programme channel="one" start="20261019130000 +0000" stop="20261019140000 +0000"><title>Guide title</title><rating><value>PG</value></rating></programme></tv>`}
	if _, e := s.ImportSource(ctx, src); e != nil {
		t.Fatal(e)
	}
	cs, e := s.Channels(ctx)
	if e != nil || len(cs) != 1 {
		t.Fatalf("channels %v %+v", e, cs)
	}
	c := cs[0]
	c.Number = "99"
	c.Favorite = true
	c.Rating = "PG"
	c.AllowedUsers = []int64{4}
	if _, e = s.SaveChannel(ctx, c); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ImportSource(ctx, src); e != nil {
		t.Fatal(e)
	}
	saved, _ := s.Channel(ctx, c.ID)
	if saved.Number != "99" || !saved.Favorite || !containsUser(saved.AllowedUsers, 4) {
		t.Fatal("refresh lost owner mapping")
	}
	user := &core.User{ID: 4, MaxContentRating: "PG"}
	g, e := s.Guide(ctx, user, now, now.Add(time.Hour))
	if e != nil || len(g) != 1 || g[0].Item.Title != "Guide title" {
		t.Fatalf("guide %v %+v", e, g)
	}
	denied, e := s.AllowedChannels(ctx, &core.User{ID: 5})
	if e != nil || len(denied) != 0 {
		t.Fatal("channel user restriction leaked")
	}
	if e = s.SavePolicy(ctx, Policy{UserID: 4, Disabled: true}); e != nil {
		t.Fatal(e)
	}
	denied, _ = s.AllowedChannels(ctx, user)
	if len(denied) != 0 {
		t.Fatal("disabled Live TV visible")
	}
	s2 := testService(t, Options{DB: s.o.DB, Now: s.o.Now})
	p, e := s2.Policy(ctx, 4)
	if e != nil || !p.Disabled {
		t.Fatal("policy not persistent")
	}
	src.Guide = "<invalid>"
	if _, e = s.ImportSource(ctx, src); e == nil {
		t.Fatal("invalid import committed")
	}
	after, _ := s.Channel(ctx, c.ID)
	if after.Number != "99" {
		t.Fatal("failed import changed channels")
	}
	sources, _ := s.Sources(ctx)
	if len(sources) != 1 || sources[0].Playlist != "" || sources[0].Guide != "" {
		t.Fatal("raw uploads leaked in source list")
	}
	// File uploads survive restart and can be refreshed without uploading again.
	rec := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/sources/owner-source/refresh", nil))
	if rec.Code != 200 {
		t.Fatalf("uploaded source refresh: %d %s", rec.Code, rec.Body.String())
	}
}

type fakePlayback struct {
	core.Playback
	prepared []*core.PlaySession
	stopped  []string
	groups   []string
}

func (f *fakePlayback) Prepare(_ context.Context, u *core.User, k core.RatingKey, c core.ClientCaps) (*core.PlaySession, error) {
	ps := &core.PlaySession{ID: fmt.Sprint(len(f.prepared) + 1), UserID: u.ID, Key: k, ClientID: c.ClientID, Info: core.MediaInfo{Duration: time.Hour}}
	f.prepared = append(f.prepared, ps)
	return ps, nil
}
func (f *fakePlayback) PrepareChannel(ctx context.Context, u *core.User, k core.RatingKey, c core.ClientCaps, g string) (*core.PlaySession, error) {
	f.groups = append(f.groups, g)
	ps, err := f.Prepare(ctx, u, k, c)
	if ps != nil {
		ps.PlaybackGroup = g
	}
	return ps, err
}
func (f *fakePlayback) InputURL(id string) string     { return "http://127.0.0.1/source/" + id }
func (f *fakePlayback) Stop(id string)                { f.stopped = append(f.stopped, id) }
func (f *fakePlayback) Sessions() []*core.PlaySession { return f.prepared }
func (f *fakePlayback) Failover(_ context.Context, id string, at time.Duration) (*core.PlaySession, error) {
	return &core.PlaySession{ID: id, Info: core.MediaInfo{Duration: time.Hour}}, nil
}

type fakeHLS struct {
	id      string
	closed  bool
	swapped bool
	offset  time.Duration
}

func (f *fakeHLS) ID() string            { return f.id }
func (f *fakeHLS) LastAccess() time.Time { return time.Time{} }
func (f *fakeHLS) Close() error          { f.closed = true; return nil }
func (f *fakeHLS) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\n00000.ts\n")
}
func (f *fakeHLS) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	fmt.Fprintf(w, "segment:%s:%d", f.id, n)
}
func (f *fakeHLS) SwapInput(_ context.Context, _ string, _ core.MediaInfo, at time.Duration) error {
	f.swapped = true
	f.offset = at
	return nil
}
func (f *fakeHLS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "video/x-matroska")
	fmt.Fprint(w, "live-mkv")
}

type fakeDASH struct {
	fakeHLS
	media []string
}

func (f *fakeDASH) ServeManifest(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<MPD><SegmentTemplate initialization="session/%s/$RepresentationID$/header" media="session/%s/$RepresentationID$/$Number$.m4s"/></MPD>`, f.id, f.id)
}
func (f *fakeDASH) ServeInit(w http.ResponseWriter, r *http.Request, rep int) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "init:%s:%d", f.id, rep)
}
func (f *fakeDASH) ServeMedia(w http.ResponseWriter, r *http.Request, rep, n int) {
	f.media = append(f.media, fmt.Sprintf("%d:%d", rep, n))
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "media:%s:%d:%d", f.id, rep, n)
}

type fakeTranscoder struct {
	core.Transcoder
	requests []core.HLSRequest
	hls      []*fakeHLS
	dash     []*fakeDASH
}

func (f *fakeTranscoder) Decide(_ core.MediaInfo, _ core.ClientCaps, o core.DecideOptions) core.Decision {
	kind := core.DecisionRemux
	if !o.DirectStream {
		kind = core.DecisionTranscode
	}
	return core.Decision{Kind: kind, Container: "mpegts", VideoCopy: o.DirectStream, AudioCopy: o.DirectStream}
}
func (f *fakeTranscoder) StartHLS(_ context.Context, r core.HLSRequest) (core.HLSSession, error) {
	f.requests = append(f.requests, r)
	if r.Protocol == core.ProtocolHTTP {
		h := &fakeHLS{id: r.SessionID}
		f.hls = append(f.hls, h)
		return h, nil
	}
	if r.Protocol == core.ProtocolDASH {
		h := &fakeDASH{fakeHLS: fakeHLS{id: r.SessionID}}
		f.dash = append(f.dash, h)
		f.hls = append(f.hls, &h.fakeHLS)
		return h, nil
	}
	h := &fakeHLS{id: r.SessionID}
	f.hls = append(f.hls, h)
	return h, nil
}
func (f *fakeTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	return &core.MediaInfo{Container: "mpegts"}, nil
}
func TestVirtualPlayoutJoinContinuityAndOwnership(t *testing.T) {
	now := time.Unix(7, 0).UTC()
	pb := &fakePlayback{}
	tc := &fakeTranscoder{}
	s := testService(t, Options{Now: func() time.Time { return now }, Playback: pb, Transcoder: tc})
	c := testChannel()
	c.Items = []Item{{Key: core.MovieKey(1), Title: "First", Rating: "G", DurationMS: 12000}, {Key: core.MovieKey(2), Title: "Second", Rating: "PG", DurationMS: 12000}}
	if _, e := s.SaveChannel(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	u := &core.User{ID: 1, StreamLimit: 1}
	info, e := s.Tune(context.Background(), u, c.ID, core.ClientCaps{})
	if e != nil {
		t.Fatal(e)
	}
	if tc.requests[0].LiveInput {
		t.Fatal("virtual-channel VOD must not permit live playlist input")
	}
	if tc.requests[0].StartOffset != 4*time.Second || tc.requests[0].Info.Duration != 12*time.Second {
		t.Fatalf("join seek wrong %+v", tc.requests[0])
	}
	req := httptest.NewRequest("GET", "/livetv/sessions/"+info.ID+"/0/index.m3u8?X-Plex-Token=owner-token", nil)
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, req, info.ID, u)
	b := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(b, "#EXT-X-DISCONTINUITY\n") || strings.Contains(b, "#EXT-X-ENDLIST") || strings.Contains(b, "X-Plex-Token=") || !strings.Contains(b, "X-Ether-Stream-Token=") {
		t.Fatalf("invalid live playlist: %d %s", rec.Code, b)
	}
	if len(pb.groups) < 2 || pb.groups[0] != pb.groups[1] {
		t.Fatal("programmes did not share live stream group")
	}
	other := httptest.NewRecorder()
	s.ServePlaylist(other, req, info.ID, &core.User{ID: 2})
	if other.Code != 403 {
		t.Fatal("another user opened session")
	}
	seg := httptest.NewRecorder()
	s.ServeSegment(seg, req, info.ID, 0, 1, u)
	if seg.Code != 200 || !strings.Contains(seg.Body.String(), ":1") {
		t.Fatal("join segment unavailable")
	}
	hs, ok := s.TranscodeFor(pb.prepared[0].ID)
	if !ok || hs.ID() != tc.hls[0].id {
		t.Fatal("failover lookup lost session")
	}
	if e = s.Failover(context.Background(), info.ID, u); e != nil || !tc.hls[0].swapped || tc.hls[0].offset != 7*time.Second {
		t.Fatalf("failover lost programme timestamp: %v", e)
	}
	if _, e = s.Tune(context.Background(), u, c.ID, core.ClientCaps{}); e != core.ErrStreamLimit {
		t.Fatalf("viewer limit bypassed: %v", e)
	}
	now = time.Unix(13, 0)
	u.MaxContentRating = "G"
	denied := httptest.NewRecorder()
	s.ServePlaylist(denied, req, info.ID, u)
	if denied.Code != 403 {
		t.Fatal("rating restriction not enforced at programme boundary")
	}
	u.MaxContentRating = ""
	if e = s.Stop(context.Background(), info.ID, u); e != nil {
		t.Fatal(e)
	}
	for _, h := range tc.hls {
		if !h.closed {
			t.Fatal("transcoder left open")
		}
	}
}

func TestLiveHTTPMatroska(t *testing.T) {
	now := time.Unix(7, 0).UTC()
	pb := &fakePlayback{}
	tc := &fakeTranscoder{}
	s := testService(t, Options{Now: func() time.Time { return now }, Playback: pb, Transcoder: tc})
	c := testChannel()
	if _, e := s.SaveChannel(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	u := &core.User{ID: 1, StreamLimit: 1}
	info, e := s.Tune(context.Background(), u, c.ID, core.ClientCaps{})
	if e != nil {
		t.Fatal(e)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/start?protocol=http", nil)
	if err := s.ServeHTTP(rec, req, info.ID, u); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "video/x-matroska" || rec.Body.String() != "live-mkv" {
		t.Fatalf("http start: %d %s %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	var sawHTTP bool
	for _, r := range tc.requests {
		if r.Protocol == core.ProtocolHTTP {
			sawHTTP = true
			if r.LiveInput {
				t.Fatal("virtual-channel HTTP must not permit live playlist input")
			}
		}
	}
	if !sawHTTP {
		t.Fatal("ServeHTTP did not start an HTTP session")
	}
	if e = s.Stop(context.Background(), info.ID, u); e != nil {
		t.Fatal(e)
	}
	for _, h := range tc.hls {
		if !h.closed {
			t.Fatal("http session left open")
		}
	}
}
func TestIPTVRelativeManifestKeysRangesRetryAndReconnect(t *testing.T) {
	var attempts atomic.Int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/hls/master.m3u8", 302)
		case "/hls/master.m3u8":
			if attempts.Add(1) < 3 {
				http.Error(w, "retry", 503)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"audio/list.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=100000\nvariant/index.m3u8?auth=source\n")
		case "/hls/variant/index.m3u8":
			w.Header().Set("Content-Type", "application/x-mpegurl")
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:50\n#EXT-X-TARGETDURATION:4\n#EXT-X-KEY:METHOD=AES-128,URI=\"../keys/key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-DISCONTINUITY\n#EXTINF:4,\n#EXT-X-BYTERANGE:4@0\n../segment.ts\n")
		case "/hls/keys/key.bin":
			// Pluto's key endpoint uses the playlist MIME type for a binary key.
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprint(w, "0123456789abcdef")
		case "/hls/segment.ts":
			if r.Header.Get("Range") != "bytes=0-3" {
				t.Error("range not forwarded")
			}
			w.Header().Set("Content-Range", "bytes 0-3/8")
			w.WriteHeader(206)
			fmt.Fprint(w, "DATA")
		default:
			fmt.Fprint(w, "init")
		}
	}))
	defer upstream.Close()
	s := testService(t, Options{HTTPClient: upstream.Client()})
	c := Channel{ID: "iptv", Name: "Owner", Number: "1", Kind: "iptv", Enabled: true, StreamURL: upstream.URL + "/redirect", Rating: "PG"}
	if _, e := s.SaveChannel(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	u := &core.User{ID: 7}
	ss, e := s.Tune(context.Background(), u, c.ID, core.ClientCaps{})
	if e != nil {
		t.Fatal(e)
	}
	request := func(key string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/livetv/sessions/"+ss.ID+"/0/index.m3u8?X-Plex-Token=view-token", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.ServeResource(rec, r, ss.ID, key, u)
		return rec
	}
	master := request("index.m3u8", nil)
	if master.Code != 200 || strings.Contains(master.Body.String(), upstream.URL) || strings.Contains(master.Body.String(), "X-Plex-Token=") || !strings.Contains(master.Body.String(), "X-Ether-Stream-Token=") {
		t.Fatalf("master rewrite failed: %d %s", master.Code, master.Body.String())
	}
	if attempts.Load() < 3 {
		t.Fatal("upstream not retried")
	}
	var variantKey string
	for _, line := range strings.Split(master.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			v, _ := url.Parse(line)
			variantKey = path.Base(v.Path)
		}
	}
	variant := request(variantKey, nil)
	b := variant.Body.String()
	if variant.Code != 200 || !strings.Contains(b, "#EXT-X-DISCONTINUITY") || !strings.Contains(b, "#EXT-X-BYTERANGE:4@0") {
		t.Fatalf("media rewrite failed %d %s", variant.Code, b)
	}
	uris := hlsURI.FindAllStringSubmatch(b, -1)
	if len(uris) != 2 {
		t.Fatalf("key/map lost: %s", b)
	}
	keyURL, _ := url.Parse(uris[0][1])
	key := request(path.Base(keyURL.Path), nil)
	if key.Code != 200 || key.Body.String() != "0123456789abcdef" || key.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("binary key not proxied: status=%d type=%q body=%s", key.Code, key.Header().Get("Content-Type"), key.Body.String())
	}
	var segmentKey string
	for _, line := range strings.Split(b, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			v, _ := url.Parse(line)
			segmentKey = path.Base(v.Path)
		}
	}
	segment := request(segmentKey, map[string]string{"Range": "bytes=0-3"})
	if segment.Code != 206 || segment.Body.String() != "DATA" || segment.Header().Get("Content-Range") == "" {
		t.Fatalf("segment range failed %d %s", segment.Code, segment.Body.String())
	}
	unknown := request("arbitrary-upstream-url", nil)
	if unknown.Code != 404 {
		t.Fatal("unregistered proxy URL accepted")
	}
	// Forced normalization uses the same authenticated Live TV playlist route.
	tc := &fakeTranscoder{}
	s.o.Transcoder = tc
	c.Transcode = true
	if _, e = s.SaveChannel(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	normalized, e := s.Tune(context.Background(), u, c.ID, core.ClientCaps{})
	if e != nil {
		t.Fatal(e)
	}
	if len(tc.requests) != 1 || tc.requests[0].Decision.Kind != core.DecisionTranscode {
		t.Fatal("IPTV transcode mode ignored")
	}
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest("GET", "/", nil), normalized.ID, u)
	if !strings.Contains(rec.Body.String(), "/relay/0.ts") {
		t.Fatalf("relay segment URI wrong: %s", rec.Body.String())
	}
}
func TestOwnerAPIAndXMLTVEscaping(t *testing.T) {
	s := testService(t, Options{Now: func() time.Time { return time.Unix(7, 0) }})
	c := testChannel()
	c.Name = "Owner & <channel>"
	data, _ := json.Marshal(c)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/channels", strings.NewReader(string(data))))
	if rec.Code != 200 {
		t.Fatalf("save api %d %s", rec.Code, rec.Body.String())
	}
	guide := httptest.NewRecorder()
	s.ServeXMLTV(guide, httptest.NewRequest("GET", "/guide.xml", nil), &core.User{ID: 1, IsOwner: true})
	d := xml.NewDecoder(strings.NewReader(guide.Body.String()))
	for {
		_, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	if !strings.Contains(guide.Body.String(), "Owner &amp; &lt;channel&gt;") {
		t.Fatal("XMLTV did not escape channel name")
	}
}

func TestPlaybackPermissionRevokedAndRawIPTVRemux(t *testing.T) {
	allowed := true
	tc := &fakeTranscoder{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		fmt.Fprint(w, "G00000000000000000000000")
	}))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client(), Transcoder: tc, CanPlay: func(*core.User, bool, time.Time) bool { return allowed }})
	c := Channel{ID: "raw", Name: "Owner transport stream", Number: "4", Kind: "iptv", Enabled: true, StreamURL: up.URL + "/stream"}
	if _, e := s.SaveChannel(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	u := &core.User{ID: 1}
	info, e := s.Tune(context.Background(), u, c.ID, core.ClientCaps{})
	if e != nil {
		t.Fatal(e)
	}
	if len(tc.requests) != 1 || tc.requests[0].Decision.Kind != core.DecisionRemux || tc.requests[0].Info.Duration != 0 {
		t.Fatal("raw HTTP stream not remuxed as live")
	}
	allowed = false
	rec := httptest.NewRecorder()
	s.ServeRelaySegment(rec, httptest.NewRequest("GET", "/", nil), info.ID, 0, u)
	if rec.Code != 403 {
		t.Fatal("viewing-hour/remote policy was not rechecked")
	}
	if _, e = s.Tune(context.Background(), u, c.ID, core.ClientCaps{}); e != core.ErrUnauthorized {
		t.Fatal("revoked user tuned")
	}
	allowed = true
	if _, e = s.SaveChannel(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Session(context.Background(), info.ID, u); e != core.ErrNotFound {
		t.Fatal("source edit retained stale live session")
	}
	if !tc.hls[0].closed {
		t.Fatal("source edit left transcoder running")
	}
}

type fakeChannelLimiter struct {
	*fakePlayback
	begun, ended int
}

func (f *fakeChannelLimiter) BeginChannel(*core.User, core.ClientCaps, string) error {
	f.begun++
	return nil
}
func (f *fakeChannelLimiter) EndChannel(string) { f.ended++ }
func TestChannelReservationReleasedOnStopAndPrepareFailure(t *testing.T) {
	pb := &fakeChannelLimiter{fakePlayback: &fakePlayback{}}
	s := testService(t, Options{Playback: pb, Now: func() time.Time { return time.Unix(7, 0) }})
	c := testChannel()
	if _, e := s.SaveChannel(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	u := &core.User{ID: 1}
	if _, e := s.Tune(context.Background(), u, c.ID, core.ClientCaps{}); e != core.ErrNotAvailable {
		t.Fatalf("missing transcoder: %v", e)
	}
	if pb.begun != 1 || pb.ended != 1 || len(s.ViewingSessions()) != 0 {
		t.Fatal("failed tune leaked a viewing reservation")
	}
	s.o.Transcoder = &fakeTranscoder{}
	info, e := s.Tune(context.Background(), u, c.ID, core.ClientCaps{})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Stop(context.Background(), info.ID, u); e != nil {
		t.Fatal(e)
	}
	if pb.begun != 2 || pb.ended != 2 {
		t.Fatal("stop leaked a viewing reservation")
	}
}

func TestAbandonedTunesReclaimedOnlyForSameViewerAndClient(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "#EXTM3U\n#EXTINF:4,\npart.ts\n") }))
	defer up.Close()
	s := testService(t, Options{HTTPClient: up.Client()})
	c, err := s.SaveChannel(context.Background(), Channel{Name: "Test", Number: "1", Kind: "iptv", Enabled: true, StreamURL: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1, StreamLimit: 1}
	caps := core.ClientCaps{ClientID: "apple-tv"}
	first, err := s.Tune(context.Background(), u, c.ID, caps)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Tune(context.Background(), u, c.ID, caps)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("new tune reused its old session")
	}
	if _, err = s.Session(context.Background(), first.ID, u); err != core.ErrNotFound {
		t.Fatalf("unused tune survived: %v", err)
	}
	if _, err = s.Tune(context.Background(), u, c.ID, core.ClientCaps{ClientID: "other-device"}); err != core.ErrStreamLimit {
		t.Fatalf("another device evaded stream limit: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest("GET", "/", nil), second.ID, u)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if _, err = s.Tune(context.Background(), u, c.ID, caps); err != core.ErrStreamLimit {
		t.Fatalf("active media session was reaped: %v", err)
	}
}

// XMLTV's stop attribute is optional; a programme without one runs until the
// channel's next programme instead of failing the whole guide import.
func TestXMLTVMissingStopAndCompactOffset(t *testing.T) {
	guide := `<tv><channel id="one"><display-name>One</display-name></channel>` +
		`<programme channel="one" start="20261019090000 +0000"><title>A</title></programme>` +
		`<programme channel="one" start="20261019100000+0000" stop="20261019110000+0000"><title>B</title></programme>` +
		`<programme channel="one" start="20261019110000 +0000"><title>Tail</title></programme></tv>`
	g, err := ParseXMLTV(strings.NewReader(guide))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Programmes) != 2 {
		t.Fatalf("programmes = %+v", g.Programmes)
	}
	a := g.Programmes[0]
	if a.Item.Title != "A" || a.End.Sub(a.Start) != time.Hour || a.Item.DurationMS != time.Hour.Milliseconds() {
		t.Fatalf("inferred stop wrong: %+v", a)
	}
	if b := g.Programmes[1]; b.Item.Title != "B" || b.Start.Hour() != 10 {
		t.Fatalf("compact offset parse wrong: %+v", b)
	}
}

// One zero-length or inverted programme (common in provider feeds) and one
// without a channel are skipped instead of rejecting the whole guide.
func TestXMLTVSkipsUnairableProgrammes(t *testing.T) {
	guide := `<tv><channel id="one"><display-name>One</display-name></channel>` +
		`<programme channel="one" start="20261019090000 +0000" stop="20261019100000 +0000"><title>A</title></programme>` +
		`<programme channel="one" start="20261019100000 +0000" stop="20261019100000 +0000"><title>Marker</title></programme>` +
		`<programme channel="one" start="20261019110000 +0000" stop="20261019103000 +0000"><title>Inverted</title></programme>` +
		`<programme start="20261019100000 +0000" stop="20261019110000 +0000"><title>Orphan</title></programme>` +
		`<programme channel="one" start="20261019100000 +0000" stop="20261019110000 +0000"><title>B</title></programme></tv>`
	g, err := ParseXMLTV(strings.NewReader(guide))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Programmes) != 2 || g.Programmes[0].Item.Title != "A" || g.Programmes[1].Item.Title != "B" {
		t.Fatalf("programmes = %+v", g.Programmes)
	}
}
