package app

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
)

// All requests traverse a TCP listener, the App handler, the mounted registry,
// and real identity/catalogue/policy/capability services. No Host fakes.
func TestCheapSurfacesMounted(t *testing.T) {
	testCheapSurfacesMounted(t, t.TempDir(), nil, nil)
}

func testCheapSurfacesMounted(t *testing.T, dir string, configure func(*config.Config), media []byte) {
	ctx := context.Background()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		if configure != nil {
			configure(c)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var appLog bytes.Buffer
	a, err := New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(&appLog, nil))})
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(appLog.String())
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	server := httptest.NewServer(a.Handler())
	t.Cleanup(server.Close)
	a.frontends.start(ctx, server.Listener.Addr())
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "cert-viewer", "cert-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	title := core.Title{Popularity: 100, VoteCount: 10000, Key: core.MovieKey(999), Type: core.TypeMovie, IDs: core.IDs{TMDB: 999}, Title: "Certification Movie", Year: 2000, ContentRating: "PG-13", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	durationMS := int64(3600000)
	if len(media) > 0 {
		durationMS = 120000
	}
	channel, err := a.livetv.SaveChannel(ctx, livetv.Channel{ID: "cert-channel", Number: "1", Name: "Cert & News", Kind: "virtual", Enabled: true, AllowedUsers: []int64{id}, Items: []livetv.Item{{Key: title.Key, Title: title.Title, DurationMS: durationMS}}})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[frontend.Protocol]string{}
	for _, p := range []frontend.Protocol{"stremio", "webdav", "m3u"} {
		tokens[p], err = grants.Issue(ctx, id, nil, string(p), core.Device{ID: "cert-" + string(p)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = a.frontends.control.PutSettings(ctx, "dlna", frontend.Settings{"viewerId": float64(id), "discovery": false}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []frontend.Protocol{"stremio", "webdav", "m3u", "dlna"} {
		if err = a.frontends.control.Toggle(ctx, p, true); err != nil {
			t.Fatal(err)
		}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	request := func(method, path, body, token string, headers map[string]string, want int) []byte {
		t.Helper()
		req, e := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		if token != "" {
			req.SetBasicAuth(fmt.Sprint(id), token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
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
		if res.StatusCode != want {
			t.Fatalf("%s mounted request: got %d want %d: %s", method, res.StatusCode, want, data)
		}
		return data
	}
	stremio := "/stremio/" + tokens["stremio"]
	m3u := "/m3u/" + tokens["m3u"]
	request("GET", "/ether/api/health", "", "", nil, 200)
	request("GET", "/stremio/invalid/manifest.json", "", "", nil, 401)
	manifest := request("GET", stremio+"/manifest.json", "", "", nil, 200)
	var decoded map[string]any
	if err = json.Unmarshal(manifest, &decoded); err != nil || decoded["id"] != "org.ether.viewer" {
		t.Fatalf("manifest: %s %v", manifest, err)
	}
	request("GET", "/stremio/"+tokens["m3u"]+"/manifest.json", "", "", nil, 401)
	if _, e := a.catalog.Get(ctx, title.Key); e != nil {
		t.Fatalf("seeded catalogue detail: %v", e)
	}
	request("GET", stremio+"/meta/movie/tmdb:999.json", "", "", nil, 200)
	// Browse must return the seeded title through the real catalogue.
	catalog := request("GET", stremio+"/catalog/movie/ether-movies.json", "", "", nil, 200)
	if !strings.Contains(string(catalog), title.Title) {
		t.Fatalf("catalog omitted fixture: %s", catalog)
	}
	streams := request("GET", stremio+"/stream/movie/tmdb:999.json", "", "", nil, 200)
	if strings.Contains(string(streams), tokens["stremio"]) {
		t.Fatal("login grant in Stremio media URL")
	}
	request("PROPFIND", "/webdav/", "", "", map[string]string{"Depth": "1"}, 401)
	dav := request("PROPFIND", "/webdav/", "", tokens["webdav"], map[string]string{"Depth": "1"}, 207)
	var tree struct {
		Responses []struct {
			Href string `xml:"href"`
		} `xml:"response"`
	}
	if err = xml.Unmarshal(dav, &tree); err != nil || len(tree.Responses) != 4 {
		t.Fatalf("DAV root: %s %v", dav, err)
	}
	request("PUT", "/webdav/file", "", tokens["webdav"], nil, 405)
	playlist := request("GET", m3u+"/playlist.m3u", "", "", nil, 200)
	if !strings.Contains(string(playlist), "/sessions/lineup/") || strings.Contains(string(playlist), tokens["m3u"]) {
		t.Fatalf("unsafe playlist: %s", playlist)
	}
	guide := request("GET", m3u+"/guide.xml", "", "", nil, 200)
	var tv struct {
		Channels []struct {
			ID string `xml:"id,attr"`
		} `xml:"channel"`
		Programmes []struct {
			Title string `xml:"title"`
		} `xml:"programme"`
	}
	if err = xml.Unmarshal(guide, &tv); err != nil || len(tv.Channels) != 1 || len(tv.Programmes) == 0 {
		t.Fatalf("XMLTV: %s %v", guide, err)
	}
	request("GET", "/dlna/description.xml", "", "", nil, 200)
	request("GET", "/dlna/ContentDirectory/description.xml", "", "", nil, 200)
	soap := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ObjectID>0</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>100</RequestedCount><SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`
	browse := request("POST", "/dlna/ContentDirectory/control", soap, "", map[string]string{"SOAPACTION": `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`, "Content-Type": "text/xml; charset=utf-8"}, 200)
	if !strings.Contains(string(browse), "view:1") {
		t.Fatalf("DLNA root: %s", browse)
	}

	var streamList struct {
		Streams []struct {
			URL string `json:"url"`
		} `json:"streams"`
	}
	if err = json.Unmarshal(streams, &streamList); err != nil || len(streamList.Streams) != 1 {
		t.Fatalf("streams: %s %v", streams, err)
	}
	var dlnaURL string
	if len(media) > 0 {
		rangeCheck := func(raw, token string) {
			t.Helper()
			u, e := url.Parse(raw)
			if e != nil {
				t.Fatal(e)
			}
			if u.IsAbs() && u.Host != strings.TrimPrefix(server.URL, "http://") {
				t.Fatalf("media origin: %s", u.Host)
			}
			data := request("GET", u.RequestURI(), "", token, map[string]string{"Range": "bytes=0-1023"}, 206)
			if !bytes.Equal(data, media[:1024]) {
				t.Fatal("range bytes differ from local media")
			}
		}
		rangeCheck(streamList.Streams[0].URL, "")
		rangeCheck("/webdav/Movies/Certification%20Movie%20(2000)/Certification%20Movie%20(2000).mkv", tokens["webdav"])
		movieBrowse := strings.Replace(soap, "<ObjectID>0</ObjectID>", "<ObjectID>view:1</ObjectID>", 1)
		result := request("POST", "/dlna/ContentDirectory/control", movieBrowse, "", map[string]string{"SOAPACTION": `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`}, 200)
		var envelope struct {
			Body struct {
				Browse struct {
					Result string `xml:"Result"`
				} `xml:"BrowseResponse"`
			} `xml:"Body"`
		}
		if err = xml.Unmarshal(result, &envelope); err != nil {
			t.Fatal(err)
		}
		var didl struct {
			Items []struct {
				Resource string `xml:"res"`
			} `xml:"item"`
		}
		if err = xml.Unmarshal([]byte(envelope.Body.Browse.Result), &didl); err != nil || len(didl.Items) != 1 || didl.Items[0].Resource == "" {
			t.Fatalf("DIDL resources: %s %v", result, err)
		}
		dlnaURL = didl.Items[0].Resource
		rangeCheck(dlnaURL, "")
		var liveURL string
		for _, line := range strings.Split(string(playlist), "\n") {
			if strings.HasPrefix(line, "http://") {
				liveURL = strings.TrimSpace(line)
				break
			}
		}
		probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		output, e := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-analyzeduration", "2000000", "-probesize", "32768", "-read_intervals", "%+1", "-show_entries", "stream=codec_name", "-of", "json", liveURL).CombinedOutput()
		if e != nil || !bytes.Contains(output, []byte("h264")) {
			t.Fatalf("ffprobe mounted lineup: %v %s", e, output)
		}
		t.Log("real file source: Stremio/WebDAV/DLNA byte ranges match; ffprobe decoded M3U/HLS")
	}
	// Channel access changes must be effective on the already mounted instance.
	channel.AllowedUsers = []int64{id + 1}
	if _, err = a.livetv.SaveChannel(ctx, channel); err != nil {
		t.Fatal(err)
	}
	hidden := request("GET", m3u+"/playlist.m3u", "", "", nil, 200)
	var lineupURL string
	for _, line := range strings.Split(string(playlist), "\n") {
		if strings.HasPrefix(line, "http://") {
			lineupURL = strings.TrimSpace(line)
			break
		}
	}
	parsedLineup, e := url.Parse(lineupURL)
	if e != nil {
		t.Fatal(e)
	}
	request("GET", parsedLineup.RequestURI(), "", "", nil, 404)
	if strings.Contains(string(hidden), "#EXTINF") {
		t.Fatal("channel policy not applied")
	}

	// A denied title must disappear from metadata and its already issued URL.
	viewer, e := a.users.ActiveUser(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	viewer.MaxContentRating = "G"
	if err = a.users.UpdateUser(ctx, *viewer); err != nil {
		t.Fatal(err)
	}
	request("GET", stremio+"/meta/movie/tmdb:999.json", "", "", nil, 404)
	issued, e := url.Parse(streamList.Streams[0].URL)
	if e != nil {
		t.Fatal(e)
	}
	request("GET", issued.RequestURI(), "", "", map[string]string{"Range": "bytes=0-1023"}, 404)
	if dlnaURL != "" {
		parsed, e := url.Parse(dlnaURL)
		if e != nil {
			t.Fatal(e)
		}
		request("GET", parsed.RequestURI(), "", "", nil, 404)
	}
	if err = a.frontends.control.PutSettings(ctx, "dlna", frontend.Settings{"viewerId": float64(id + 10000), "discovery": false}); err != nil {
		t.Fatal(err)
	}
	request("GET", "/dlna/description.xml", "", "", nil, 403)
	if err = a.frontends.control.PutSettings(ctx, "dlna", frontend.Settings{"viewerId": float64(id), "discovery": false}); err != nil {
		t.Fatal(err)
	}
	viewer.MaxContentRating = ""
	if err = a.users.UpdateUser(ctx, *viewer); err != nil {
		t.Fatal(err)
	}
	if err = grants.RevokeViewer(ctx, id); err != nil {
		t.Fatal(err)
	}
	request("GET", stremio+"/manifest.json", "", "", nil, 401)
	request("GET", m3u+"/guide.xml", "", "", nil, 401)
	request("GET", issued.RequestURI(), "", "", nil, 404)
	request("PROPFIND", "/webdav/", "", tokens["webdav"], map[string]string{"Depth": "1"}, 401)
	for _, p := range []frontend.Protocol{"stremio", "webdav", "m3u", "dlna"} {
		if err = a.frontends.control.Toggle(ctx, p, false); err != nil {
			t.Fatal(err)
		}
		if _, mounted := a.frontends.registry.Instance(p); mounted {
			t.Fatalf("%s still mounted", p)
		}
		request("GET", "/"+string(p)+"/description.xml", "", "", nil, 404)
		request("GET", "/ether/api/health", "", "", nil, 200)
		if err = a.frontends.control.Toggle(ctx, p, true); err != nil {
			t.Fatal(err)
		}
		if _, mounted := a.frontends.registry.Instance(p); !mounted {
			t.Fatalf("%s did not remount", p)
		}
	}
	request("GET", "/dlna/description.xml", "", "", nil, 200)
	t.Log("mounted: manifest/catalog/meta/stream, DAV tree, lineup/XMLTV, UPnP description/SOAP, policy/revocation/toggles/health")
}
