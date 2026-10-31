package spike

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestParseFileName(t *testing.T) {
	cases := []struct {
		in    string
		title string
		year  int
	}{
		{"/m/The.Signal.1999.1080p.BluRay.x264.mkv", "The Signal", 1999},
		{"/m/Big Buck Bunny (2008).mp4", "Big Buck Bunny", 2008},
		{"/m/Sintel_2010_720p.mkv", "Sintel", 2010},
		{"/m/Blade.Runner.2049.2017.2160p.mkv", "Blade Runner 2049", 2017},
		{"/m/home-video.mov", "home-video", 0},
		{"/m/Tears.of.Steel.1080p.WEB-DL.mkv", "Tears of Steel", 0},
	}
	for _, c := range cases {
		title, year := ParseFileName(c.in)
		if title != c.title || year != c.year {
			t.Errorf("%s: got %q %d, want %q %d", c.in, title, year, c.title, c.year)
		}
	}
}

func TestParseProbeMP4(t *testing.T) {
	b, err := os.ReadFile("testdata/ffprobe_mp4.json")
	if err != nil {
		t.Fatal(err)
	}
	info, err := ParseProbe(b, ".mp4")
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != "mp4" || info.Duration != 20*time.Second || info.Size == 0 || info.Estimated {
		t.Fatalf("info: %+v", info)
	}
	v := info.Video()
	if v == nil || v.Codec != "h264" || v.Width != 1280 || v.Height != 720 || v.BitDepth != 8 || v.FrameRate != 24 {
		t.Fatalf("video: %+v", v)
	}
	if len(info.Streams) != 2 || info.Streams[1].Codec != "aac" || info.Streams[1].SampleRate != 44100 {
		t.Fatalf("streams: %+v", info.Streams)
	}
}

func TestParseProbeMKVHDR(t *testing.T) {
	b, err := os.ReadFile("testdata/ffprobe_mkv_hdr.json")
	if err != nil {
		t.Fatal(err)
	}
	info, err := ParseProbe(b, ".mkv")
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != "mkv" || info.BitrateKbps != 44444 || len(info.Streams) != 4 {
		t.Fatalf("info: %+v", info)
	}
	v := info.Streams[0]
	if v.Codec != "hevc" || v.BitDepth != 10 || v.ColorTransfer != "smpte2084" || !v.DolbyVision || v.BitrateKbps != 45000 ||
		v.FrameRate < 23.97 || v.FrameRate > 23.98 || v.Language != "eng" {
		t.Fatalf("video: %+v", v)
	}
	if a := info.Streams[1]; a.Codec != "truehd" || a.Channels != 8 || !a.Default || a.Title != "TrueHD Atmos 7.1" {
		t.Fatalf("audio: %+v", a)
	}
	if s := info.Streams[3]; s.Codec != "pgs" || !s.Forced || s.Type != "subtitle" {
		t.Fatalf("subtitle: %+v", s)
	}
	if _, err := ParseProbe([]byte("nope"), ".mkv"); err == nil {
		t.Fatal("expected error")
	}
}

func TestMemStateWatched(t *testing.T) {
	s := newMemState()
	ctx := context.Background()
	k := core.LocalKey(1)
	dur := 100 * time.Second
	s.Progress(ctx, 1, k, 10*time.Second, dur, "playing")
	ip, _ := s.InProgress(ctx, 1, 10)
	if len(ip) != 1 || ip[0].ViewOffset != 10*time.Second {
		t.Fatalf("in progress: %+v", ip)
	}
	// Crossing 90% marks watched once, however many timelines follow.
	for _, off := range []time.Duration{91, 95, 99} {
		s.Progress(ctx, 1, k, off*time.Second, dur, "playing")
	}
	ws, _ := s.Get(ctx, 1, k)
	if ws.ViewCount != 1 || ws.ViewOffset != 0 {
		t.Fatalf("watched: %+v", ws)
	}
	if h, _ := s.History(ctx, 1, time.Time{}, 10); len(h) != 1 || !h[0].Completed {
		t.Fatalf("history: %+v", h)
	}
	s.Unscrobble(ctx, 1, k)
	if ws, _ := s.Get(ctx, 1, k); ws.ViewCount != 0 {
		t.Fatal("unscrobble")
	}
	if err := s.Rate(ctx, 1, k, 11); err == nil {
		t.Fatal("rating out of range accepted")
	}
	other, _ := s.Get(ctx, 2, k)
	if other.ViewCount != 0 {
		t.Fatal("state leaked across users")
	}
}

func TestMemPlaylists(t *testing.T) {
	p := newMemPlaylists()
	ctx := context.Background()
	a, b, c := core.LocalKey(1), core.LocalKey(2), core.LocalKey(3)
	pl, _ := p.Create(ctx, 1, "x", []core.RatingKey{a, b, c})
	if err := p.Move(ctx, 1, pl.Key, 0, 2); err != nil {
		t.Fatal(err)
	}
	got, _ := p.Get(ctx, 1, pl.Key)
	if fmt.Sprint(got.Items) != fmt.Sprint([]core.RatingKey{b, c, a}) {
		t.Fatalf("move: %v", got.Items)
	}
	if err := p.Remove(ctx, 1, pl.Key, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(ctx, 2, pl.Key); err == nil {
		t.Fatal("other user can read playlist")
	}
	if err := p.Delete(ctx, 1, pl.Key); err != nil {
		t.Fatal(err)
	}
}

// newTestLibrary writes two small "video" files and builds a library over
// them without ffprobe.
func newTestLibrary(t *testing.T, opts Options) (*Library, []string) {
	t.Helper()
	dir := t.TempDir()
	var files []string
	for _, name := range []string{"Big.Buck.Bunny.2008.720p.mp4", "Sintel.2010.1080p.mkv"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Repeat(name, 500)), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	cfg := config.Config{}
	cfg.Server.FriendlyName = "Spike"
	cfg.Server.MachineIdentifier = "spike-machine"
	cfg.Server.DefaultStreamLimit = 1
	cfg.Spike.LocalFiles = append(files, filepath.Join(dir, "missing.mkv"))
	cfg.Plex.Token = "owner-token"
	opts.DisableProbe = true
	lib, err := New(context.Background(), func() config.Config { return cfg }, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lib.Close() })
	return lib, files
}

func TestLibraryCatalog(t *testing.T) {
	lib, files := newTestLibrary(t, Options{})
	ctx := context.Background()
	page, err := lib.Catalog().Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortTitle})
	if err != nil || page.Total != 2 || page.Items[0].Title != "Big Buck Bunny" || page.Items[1].Year != 2010 {
		t.Fatalf("browse: %+v %v", page, err)
	}
	k := fileKey(files[0])
	if k.Kind() != core.KindLocal || !k.Valid() {
		t.Fatalf("key %v", k)
	}
	if got, err := lib.Catalog().Get(ctx, k); err != nil || got.Title != "Big Buck Bunny" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := lib.Catalog().Get(ctx, core.LocalKey(42)); err != core.ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	res, _ := lib.Catalog().Search(ctx, "sint", []core.MediaType{core.TypeMovie}, 10)
	if len(res) != 1 || res[0].Title != "Sintel" {
		t.Fatalf("search: %+v", res)
	}
	fc, _ := lib.Catalog().Facet(ctx, core.TypeMovie, "firstCharacter")
	if len(fc) != 2 || fc[0].Key != "B" {
		t.Fatalf("firstCharacter: %+v", fc)
	}
	dec, _ := lib.Catalog().Facet(ctx, core.TypeMovie, "decade")
	if len(dec) != 1 || dec[0].Key != "2000" || dec[0].Count != 1 {
		// 2008 -> 2000s, 2010 -> 2010s
		if len(dec) != 2 {
			t.Fatalf("decade: %+v", dec)
		}
	}
	info := lib.Playback().Estimate(ctx, &page.Items[0], core.ClientCaps{})
	if !info.Estimated || info.Container != "mp4" || info.Size == 0 {
		t.Fatalf("estimate: %+v", info)
	}
}

func TestPlaybackSessions(t *testing.T) {
	lib, files := newTestLibrary(t, Options{})
	ctx := context.Background()
	u := &core.User{ID: 1}
	k := fileKey(files[0])
	ps, err := lib.Playback().Prepare(ctx, u, k, core.ClientCaps{ClientID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := lib.Playback().Prepare(ctx, u, k, core.ClientCaps{ClientID: "a"})
	if again.ID != ps.ID {
		t.Fatal("session not reused")
	}
	// Default stream limit is 1.
	if _, err := lib.Playback().Prepare(ctx, u, fileKey(files[1]), core.ClientCaps{ClientID: "b"}); err != core.ErrStreamLimit {
		t.Fatalf("limit: %v", err)
	}
	if _, err := lib.Playback().Prepare(ctx, u, core.LocalKey(5), core.ClientCaps{}); err != core.ErrNotAvailable {
		t.Fatalf("unknown: %v", err)
	}
	if lib.Playback().InputURL(ps.ID) != files[0] {
		t.Fatal("input url")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=0-3")
	lib.Playback().ServeRange(rec, req, ps.ID)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "Big." {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}
	lib.playback.expire(time.Now().Add(time.Hour), time.Minute)
	if _, ok := lib.Playback().Session(ps.ID); ok {
		t.Fatal("idle session not expired")
	}
}

func TestAuth(t *testing.T) {
	lib, _ := newTestLibrary(t, Options{Tokens: []string{"extra"}})
	ctx := context.Background()
	if u, err := lib.Auth().Authenticate(ctx, "owner-token"); err != nil || !u.IsOwner {
		t.Fatalf("owner: %v", err)
	}
	if _, err := lib.Auth().Authenticate(ctx, "extra"); err != nil {
		t.Fatalf("extra: %v", err)
	}
	if _, err := lib.Auth().Authenticate(ctx, "nope"); err != core.ErrUnauthorized {
		t.Fatalf("bad token: %v", err)
	}
	anyLib, _ := newTestLibrary(t, Options{AcceptAnyToken: true})
	if _, err := anyLib.Auth().Authenticate(ctx, "whatever"); err != nil {
		t.Fatal(err)
	}
	if _, err := anyLib.Auth().Authenticate(ctx, ""); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestDecide(t *testing.T) {
	tr := &transcoder{}
	info := core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Height: 2160},
		{Index: 1, Type: "audio", Codec: "eac3", Default: true},
	}}
	atv := core.ClientCaps{VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "eac3"}, Containers: []string{"mkv", "mp4"}, MaxHeight: 2160}
	if d := tr.Decide(info, atv, core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1}); d.Kind != core.DecisionDirectPlay {
		t.Fatalf("atv: %+v", d)
	}
	web := core.ClientCaps{VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "eac3"}, Containers: []string{"mp4"}, MaxHeight: 2160}
	if d := tr.Decide(info, web, core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1}); d.Kind != core.DecisionRemux {
		t.Fatalf("remux: %+v", d)
	}
	low := core.ClientCaps{VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, Containers: []string{"mp4"}, MaxHeight: 1080}
	if d := tr.Decide(info, low, core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1}); d.Kind != core.DecisionTranscode || d.VideoCodec != "h264" || d.AudioCodec != "aac" || d.MaxHeight != 1080 {
		t.Fatalf("transcode: %+v", d)
	}
}

// TestPlexIntegration runs the real Plex front end over the spike.
func TestPlexIntegration(t *testing.T) {
	lib, files := newTestLibrary(t, Options{})
	srv, closeBroker, err := lib.OpenPlex()
	if err != nil {
		t.Fatal(err)
	}
	defer closeBroker()
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	get := func(path string, hdr ...string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("X-Plex-Token", "owner-token")
		req.Header.Set("X-Plex-Client-Identifier", "it")
		req.Header.Set("Accept", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	decode := func(resp *http.Response) map[string]any {
		defer resp.Body.Close()
		var v map[string]map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v["MediaContainer"]
	}
	secs := decode(get("/library/sections"))
	if secs["size"].(float64) != 1 {
		t.Fatalf("sections: %v", secs)
	}
	all := decode(get("/library/sections/1/all"))
	if all["totalSize"].(float64) != 2 {
		t.Fatalf("all: %v", all)
	}
	key := fileKey(files[0]).String()
	dec := decode(get("/video/:/transcode/universal/decision?path=/library/metadata/"+key+"&directPlay=1&directStream=1",
		"X-Plex-Product", "Plex for Apple TV", "X-Plex-Platform", "tvOS"))
	if dec["generalDecisionCode"].(float64) != 1000 {
		t.Fatalf("decision: %v", dec)
	}
	part := dec["Metadata"].([]any)[0].(map[string]any)["Media"].([]any)[0].(map[string]any)["Part"].([]any)[0].(map[string]any)
	resp := get(part["key"].(string), "Range", "bytes=0-2", "X-Plex-Product", "Plex for Apple TV", "X-Plex-Platform", "tvOS")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "Big" {
		t.Fatalf("direct play: %d %q", resp.StatusCode, body)
	}
	get("/:/timeline?ratingKey=" + key + "&state=playing&time=5000&duration=10000").Body.Close()
	ws, _ := lib.State().Get(context.Background(), 1, fileKey(files[0]))
	if ws.ViewOffset != 5*time.Second {
		t.Fatalf("timeline state: %+v", ws)
	}
	get("/:/timeline?ratingKey=" + key + "&state=stopped&time=9500&duration=10000").Body.Close()
	if ws, _ := lib.State().Get(context.Background(), 1, fileKey(files[0])); ws.ViewCount != 1 {
		t.Fatalf("not watched: %+v", ws)
	}
	if len(lib.Playback().Sessions()) != 0 {
		t.Fatal("session not stopped")
	}
}
