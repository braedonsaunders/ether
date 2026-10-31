package mediabrowser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/conform/specs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtraETagIsInformationalOnlyForDirectRangeStreams(t *testing.T) {
	p := &Protocol{Dialect: "jellyfin"}
	ref := conform.Response{Status: 206, Kind: "binary-range", RangeKind: "start", BodyLength: 4, Headers: http.Header{"Content-Range": {"bytes 0-3/100"}, "Content-Length": {"4"}}}
	target := ref
	target.Headers = ref.Headers.Clone()
	target.Headers.Set("ETag", `"representation"`)
	request := conform.Request{Method: "GET", Path: "/Videos/${item_id}/stream?Static=true", RangeKind: "start"}
	if report := p.CompareRequest(request, ref, target); report.Blocking() != 0 || len(report.Differences) != 1 || report.Differences[0].Kind != conform.KindValue {
		t.Fatal(report)
	}
	if report := p.CompareRequest(request, target, ref); report.Blocking() == 0 {
		t.Fatal("missing target ETag was ignored")
	}
	request.Path = "/Videos/${item_id}/master.m3u8"
	if report := p.CompareRequest(request, ref, target); report.Blocking() == 0 {
		t.Fatal("non-stream ETag was ignored")
	}
}

func TestLoadFetchAlignAndRedact(t *testing.T) {
	dir := t.TempDir()
	profile := `{"protocol":"jellyfin","client":"tv","headers":{"Accept":"application/json"},"flows":{"home":[{"name":"items","method":"GET","path":"/Items"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "tv.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("jellyfin", dir)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Authorization"), `Token="secret"`) {
				t.Error("missing auth")
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{"Id": id, "ProviderIds": map[string]string{"Tmdb": "42"}}}, "AccessToken": "secret", "ServerId": "server", "Path": "/private/file", "LastPlaybackCheckIn": "2026-01-02T03:04:05Z"})
		}))
	}
	ref := serve("reference")
	defer ref.Close()
	target := serve("target")
	defer target.Close()
	ids := p.Discover(context.Background(), ref.Client(), ref.URL, "secret", "tv", nil)
	aligned := p.Discover(context.Background(), target.Client(), target.URL, "secret", "tv", &ids)
	if aligned.Values["id:reference"] != "target" {
		t.Fatalf("alignment: %#v", aligned)
	}
	corpus := p.Corpus("tv")
	if len(corpus) != 1 {
		t.Fatal(corpus)
	}
	resp, err := p.Fetch(context.Background(), ref.Client(), ref.URL, "secret", corpus[0], "tv", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	redacted := string(p.Redact(resp).Body)
	for _, secret := range []string{"secret", "/private/file", "server", "2026-01-02"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("not redacted: %s", redacted)
		}
	}
}

func TestShippedProfilesLoad(t *testing.T) {
	for _, name := range []string{"jellyfin", "emby", "silo"} {
		t.Run(name, func(t *testing.T) {
			p, err := Load(name, filepath.Join("..", "profiles", name))
			if err != nil {
				t.Fatal(err)
			}
			for id := range p.Clients() {
				if len(p.Corpus(id)) == 0 && p.profiles[id].ProfileStatus != "unverified: needs owner capture" {
					t.Errorf("empty corpus for %s", id)
				}
			}
		})
	}
}
func TestAccessTokenHeaderTemplate(t *testing.T) {
	dir := t.TempDir()
	body := `{"protocol":"emby","client":"Emby web","headers":{"X-Emby-Token":"${access_token}"},"flows":{"connect":[{"name":"ping","method":"GET","path":"/ping"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "web.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("emby", dir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "secret" {
			t.Error("token not substituted")
		}
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	if _, err = p.Fetch(context.Background(), server.Client(), server.URL, "secret", p.Corpus("web")[0], "web", nil, true); err != nil {
		t.Fatal(err)
	}
}
func TestBinaryRangeFixturesDoNotContainMedia(t *testing.T) {
	dir := t.TempDir()
	profile := `{"protocol":"emby","client":"test","flows":{"play_direct":[{"name":"stream","method":"GET","path":"/video/stream"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "web.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("emby", dir)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(ch byte) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, "movie.mp4", time.Time{}, bytes.NewReader(bytes.Repeat([]byte{ch}, 200000)))
		}))
	}
	reference := serve('A')
	defer reference.Close()
	target := serve('B')
	defer target.Close()
	corpus := p.Corpus("web")
	if len(corpus) != 3 {
		t.Fatalf("ranges: %d", len(corpus))
	}
	for _, req := range corpus {
		a, err := p.Fetch(context.Background(), reference.Client(), reference.URL, "", req, "web", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		b, err := p.Fetch(context.Background(), target.Client(), target.URL, "", req, "web", nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if a.Kind != "binary-range" || len(a.Body) != 0 || a.SHA256 == "" {
			t.Fatalf("unsafe fixture: %+v", a)
		}
		if got := p.Compare(a, b); got.Blocking() != 0 {
			t.Fatalf("range compare: %+v", got)
		}
		if a.SHA256 == b.SHA256 {
			t.Fatal("different media have same hash")
		}
		if err := conform.SaveFixture(dir, req.Name, a); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(filepath.Join(dir, req.Name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(saved, []byte("QUFBQUFB")) {
			t.Fatal("fixture contains media data")
		}
	}
}
func TestKnownEmbySuffixDefectIsTruncated(t *testing.T) {
	dir := t.TempDir()
	profile := `{"protocol":"emby","client":"test","flows":{"play_direct":[{"name":"stream","method":"GET","path":"/video/stream"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "web.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load("emby", dir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-65536/200000")
		w.Header().Set("Content-Length", "65537")
		w.WriteHeader(206)
		w.Write(bytes.Repeat([]byte{'x'}, 65537))
	}))
	defer server.Close()
	req := p.Corpus("web")[2]
	r, err := p.Fetch(context.Background(), server.Client(), server.URL, "", req, "web", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || r.BodyLength != 65537 || len(r.Body) != 0 || r.ReferenceDefect == nil {
		t.Fatalf("invalid defect capture: %+v", r)
	}
	sum := sha256.Sum256(bytes.Repeat([]byte{'x'}, 65536))
	if r.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("hash did not cover bounded prefix")
	}
}

func TestEmbyUnconfiguredReferenceAddresses(t *testing.T) {
	p := &Protocol{Dialect: "emby"}
	reference := conform.Response{Status: 200, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"Id":"id","ServerName":"lab","Version":"4.10.2.0","LocalAddresses":[],"RemoteAddresses":[]}`)}
	target := conform.Response{Status: 200, Headers: reference.Headers, Body: []byte(`{"Id":"id","ServerName":"ether","Version":"4.10.2.0","LocalAddress":"http://ether","WanAddress":"http://ether","LocalAddresses":["http://ether"],"RemoteAddresses":["http://ether"]}`)}
	report := p.Compare(reference, target)
	if report.Blocking() != 0 || len(report.ReferenceDefects) != 1 {
		t.Fatal(report)
	}
	target.Body = []byte(`{"Id":"id","ServerName":"ether","Version":"4.10.2.0"}`)
	report = p.Compare(reference, target)
	if len(report.ReferenceDefects) != 0 {
		t.Fatal("accepted missing target addresses", report)
	}
}

func TestEmbyUnprovenIndependenceIsNotBlocking(t *testing.T) {
	p := &Protocol{Dialect: "emby"}
	reference := conform.Response{Status: 200, Headers: http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}}, Body: []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=1\nmain.m3u8\n")}
	target := conform.Response{Status: 200, Headers: reference.Headers, Body: []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=1\nmain.m3u8\n")}
	report := p.CompareRequest(conform.Request{Method: "GET", Path: "/Videos/item/master.m3u8"}, reference, target)
	if report.Blocking() != 0 {
		t.Fatal(report.Differences)
	}
	jellyfin := &Protocol{Dialect: "jellyfin"}
	if jellyfin.CompareRequest(conform.Request{Method: "GET", Path: "/Videos/item/master.m3u8"}, reference, target).Blocking() == 0 {
		t.Fatal("Jellyfin lost the independence difference")
	}
}

func TestPlaybackObservationUpdatesAndClearsSession(t *testing.T) {
	p := &Protocol{Dialect: "jellyfin"}
	vars := map[string]string{"play_session_id": "old", "media_source_id": "old"}
	req := conform.Request{Method: "POST", Path: "/Items/${item_id}/PlaybackInfo"}
	p.Observe(req, conform.Response{Status: 200, Body: []byte(`{"PlaySessionId":"session-1","MediaSources":[{"Id":"source_1"}]}`)}, vars)
	if vars["play_session_id"] != "session-1" || vars["media_source_id"] != "source_1" {
		t.Fatal(vars)
	}
	p.Observe(req, conform.Response{Status: 200, Body: []byte(`{"PlaySessionId":"../bad","MediaSources":[{"Id":"source?bad"}]}`)}, vars)
	if vars["play_session_id"] != "" || vars["media_source_id"] != "" {
		t.Fatal(vars)
	}
	vars["play_session_id"] = "old"
	p.Observe(req, conform.Response{Status: 503}, vars)
	if vars["play_session_id"] != "" {
		t.Fatal(vars)
	}
}

func TestJellyfinUserItemRouteAndBlurHashKeys(t *testing.T) {
	doc, err := openapi.Parse([]byte(`{"openapi":"3.0.0","paths":{"/Items/{itemId}":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object","properties":{"MediaSources":{"type":"array","items":{"type":"object","properties":{"TranscodingUrl":{"type":"string"},"TranscodingContainer":{"type":"string"}}}}}}}}}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := &Protocol{Dialect: "jellyfin", spec: doc}
	p.specOnce.Do(func() {})
	ref := conform.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"MediaSources":[{}],"ImageBlurHashes":{"Primary":{"tag-a":"hash"}}}`)}
	target := conform.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"MediaSources":[{"TranscodingUrl":"https://ether.example/master.m3u8","TranscodingContainer":"ts"}],"ImageBlurHashes":{"Primary":{"tag-b":"hash"}}}`)}
	report := p.CompareRequest(conform.Request{Method: "GET", Path: "/Users/${user_id}/Items/${item_id}"}, ref, target)
	if report.Blocking() != 0 {
		t.Fatal(report.Differences)
	}
}

func TestCompareRequestClassifiesOnlySchemaValidExtras(t *testing.T) {
	doc, err := openapi.Parse([]byte(`{"openapi":"3.0.0","paths":{"/Items":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object","properties":{"Items":{"type":"array","items":{"type":"object","properties":{"Overview":{"type":"string"}}}}}}}}}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := &Protocol{Dialect: "emby", spec: doc}
	p.specOnce.Do(func() {})
	response := func(body string) conform.Response {
		return conform.Response{Status: 200, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(body)}
	}
	ref := response(`{"Items":[{}]}`)
	for _, tc := range []struct {
		body     string
		blocking int
	}{
		{`{"Items":[{"Overview":"one"}]}`, 0},
		{`{"Items":[{"Overview":"one"},{"Overview":7}]}`, 1},
		{`{"Items":[{"Unknown":"x"}]}`, 1},
	} {
		report := p.CompareRequest(conform.Request{Method: "GET", Path: "/Items"}, ref, response(tc.body))
		if report.Blocking() != tc.blocking {
			t.Errorf("%s: %#v", tc.body, report)
		}
	}
}

func TestPinnedEmbySchemaRecognizesRouteExtras(t *testing.T) {
	data, err := openapi.CachedSpecManifest(specs.Manifest, "emby", "")
	if err != nil {
		t.Skipf("pinned spec cache unavailable: %v", err)
	}
	doc, err := openapi.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ method, path, body, field string }{
		{"GET", "/Users/123/Items/456", `{"Overview":"text"}`, "$/Overview"},
		{"GET", "/Users/123/Items", `{"Items":[{"Genres":["Drama"]}]}`, "$/Items[]/Genres[]"},
		{"POST", "/Items/456/PlaybackInfo", `{"MediaSources":[{"TranscodingSubProtocol":"hls"}]}`, "$/MediaSources[]/TranscodingSubProtocol"},
	}
	for _, tc := range tests {
		if !doc.ValidExtra(tc.method, tc.path, 200, "application/json", []byte(tc.body), tc.field) {
			t.Errorf("schema did not recognize %s %s: %s", tc.method, tc.path, tc.field)
		}
	}
}
