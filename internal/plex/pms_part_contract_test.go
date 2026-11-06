package plex

import (
	"bufio"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPartDownloadOptionAndFilename(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	path := "/library/parts/1000000000000550/1759276800/A%2BB.mkv"
	for _, value := range []string{"true", "2", "-1", "", "%20", "%2B", "1.5", "%2B%2B1"} {
		r := f.get(t, path+"?download="+value)
		if r.Code != http.StatusBadRequest {
			t.Errorf("download=%q: %d", value, r.Code)
		}
	}
	for _, value := range []string{"1", "01", "%2B01", "0000001"} {
		r := f.get(t, path+"?download="+value)
		if r.Code != 200 {
			t.Fatalf("download=%s: %d %s", value, r.Code, r.Body.String())
		}
		cd := r.Header().Get("Content-Disposition")
		if !strings.Contains(cd, "filename*=UTF-8''A%2BB%2Emkv") || !strings.Contains(cd, `filename="A_B.mkv"`) {
			t.Fatalf("disposition %q", cd)
		}
	}
}

func TestPlexPartWithRealPlaybackConditionalRange(t *testing.T) {
	f, _ := issue94Fixture(t, true, "open")
	path := "/library/parts/1000000000000550/1759276800/not-the-stored-name.mp4"
	for _, conditional := range []string{"If-Range", "If-None-Match", "If-Modified-Since", "If-Match"} {
		r := f.get(t, path, "Range", "bytes=-1", conditional, `"unrelated"`, "X-Plex-Client-Identifier", "part-probe")
		if r.Code != 206 || r.Body.Len() != 1 || r.Header().Get("Content-Type") != "video/x-matroska" {
			t.Fatalf("%s status=%d headers=%v body=%s", conditional, r.Code, r.Header(), r.Body.String())
		}
	}
	r := f.get(t, path, "Range", "bytes=5-4", "X-Plex-Client-Identifier", "part-probe")
	if r.Code != 416 || r.Header().Get("Accept-Ranges") != "" || r.Header().Get("Content-Range") != "" || r.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("416 status=%d headers=%v", r.Code, r.Header())
	}
}

func TestPartRouteArity(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for path, want := range map[string]int{
		"/library/parts/1000000000000550":                      200,
		"/library/parts/1000000000000550/anything":             200,
		"/library/parts/1000000000000550/123/anything":         200,
		"/library/parts/1000000000000550/not-a-stamp/file.mkv": 404,
		"/library/parts/1000000000000550/123/file.mkv/extra":   404,
	} {
		r := f.get(t, path)
		if r.Code != want {
			t.Errorf("%s: %d want %d", path, r.Code, want)
		}
	}
}

// Two HTTP requests share the same real socket. Exact response lengths keep
// the second seek independent of the first response's file coordinates.
func TestPlexPartPersistentSocketSeeks(t *testing.T) {
	f, _ := issue94Fixture(t, true, "open")
	srv := httptest.NewServer(f.srv)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	conn, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	path := "/library/parts/1000000000000550/1759276800/file.mkv"
	for _, value := range []string{"bytes=0-9", "bytes=-10"} {
		_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nX-Plex-Token: %s\r\nX-Plex-Client-Identifier: socket-probe\r\nRange: %s\r\n\r\n", path, u.Host, ownerToken, value)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 206 || len(body) != 10 || response.Close {
			t.Fatalf("range %s status=%d len=%d close=%v err=%v", value, response.StatusCode, len(body), response.Close, err)
		}
	}
}

func TestPartAcquisitionErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{core.ErrNotAvailable, 404}, {core.ErrNotFound, 404}, {core.ErrSourceUnavailable, 502}, {errors.New("upstream transport failed"), 502},
	} {
		f := newFixture()
		f.playback.failures[fightClub.Key] = tc.err
		r := f.get(t, "/library/parts/1000000000000550/1/file.mkv")
		f.srv.Close()
		if r.Code != tc.want {
			t.Errorf("%v: %d want %d", tc.err, r.Code, tc.want)
		}
	}
}

func TestVideoProbeFactsReachDecision(t *testing.T) {
	for _, tc := range []struct {
		order      string
		interlaced bool
		want       string
	}{{"", false, ""}, {"unknown", false, ""}, {"progressive", false, "progressive"}, {"tt", true, "interlaced"}} {
		f := newFixture()
		f.playback.info.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
		video := &f.playback.info.Streams[0]
		video.FieldOrder, video.Interlaced = tc.order, tc.interlaced
		video.ColorRange, video.ChromaLocation, video.CodedWidth, video.CodedHeight = "tv", "left", 1920, 1088
		r := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&hasMDE=1", jsonAccept...)
		f.srv.Close()
		if r.Code != 200 {
			t.Fatalf("decision %d %s", r.Code, r.Body.String())
		}
		mc := decodeJSON(t, r)
		streams := mc["Metadata"].([]any)[0].(map[string]any)["Media"].([]any)[0].(map[string]any)["Part"].([]any)[0].(map[string]any)["Stream"].([]any)
		st := streams[0].(map[string]any)
		if tc.want == "" {
			if _, ok := st["scanType"]; ok {
				t.Errorf("unexpected scanType: %v", st)
			}
		} else if st["scanType"] != tc.want {
			t.Errorf("scanType=%v want%s", st["scanType"], tc.want)
		}
		if st["colorRange"] != "tv" || st["chromaLocation"] != "left" || st["codedWidth"] != float64(1920) || st["codedHeight"] != float64(1088) {
			t.Fatalf("probe facts: %v", st)
		}
	}
}

func TestSyncContextProjection(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, tc := range []struct {
		path string
		want any
	}{
		{"/hubs", true},
		{"/hubs/sections/1", true},
		{"/hubs/continueWatching/items", true},
		{"/hubs/metadata/1000000000000550/related", true},
		{"/hubs/metadata/1000000000000550/postplay", false},
		{"/hubs/search?query=Fjord", nil},
		{"/library/metadata/" + core.CollectionKey(8091).String() + "/children", true},
	} {
		r := f.get(t, tc.path, jsonAccept...)
		if r.Code != 200 {
			t.Fatalf("%s status%d %s", tc.path, r.Code, r.Body.String())
		}
		got := decodeJSON(t, r)["allowSync"]
		if got != tc.want {
			t.Errorf("%s allowSync=%v want %v", tc.path, got, tc.want)
		}
	}
	r := f.get(t, "/hubs", "Accept", "application/json", "X-Plex-Token", friendToken)
	if r.Code != 200 || decodeJSON(t, r)["allowSync"] != false {
		t.Fatalf("friend grant: %d %s", r.Code, r.Body.String())
	}
}
