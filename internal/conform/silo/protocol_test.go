package silo

import (
	"context"
	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSiloBearerProfile(t *testing.T) {
	dir := t.TempDir()
	body := `{"protocol":"silo","client":"Silo","flows":{"connect":[{"name":"health","method":"GET","path":"/api/v2/health"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "apple.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing bearer authorization")
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err = p.Fetch(context.Background(), srv.Client(), srv.URL, "secret", p.Corpus("apple")[0], "apple", nil, true); err != nil {
		t.Fatal(err)
	}
}

func TestShippedProfilesSendDeviceIdentity(t *testing.T) {
	p, err := Load("../profiles/silo")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"silo-apple", "silo-android"} {
		requests := p.Corpus(name)
		if len(requests) == 0 {
			t.Fatal("missing profile", name)
		}
		for _, request := range requests {
			if got := request.Headers[name+"-json"].Get("X-Silo-Device-Id"); got != "${device_id}" {
				t.Fatalf("%s %s device header = %q", name, request.Path, got)
			}
		}
	}
}

func TestSchemaValidatedExtraFieldsRemainInformational(t *testing.T) {
	if _, err := openapi.CachedSpec("../specs/manifest.json", "silo", ""); os.IsNotExist(err) {
		t.Skip("Silo OpenAPI cache is empty")
	} else if err != nil {
		t.Fatal(err)
	}
	p, err := Load("../profiles/silo")
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	reference := conform.Response{Status: 200, Headers: headers, Body: []byte(`{}`)}
	target := conform.Response{Status: 200, Headers: headers, Body: []byte(`{"original_language":"en","not_a_contract_field":"unexpected"}`)}
	report := p.CompareRequest(conform.Request{Method: "GET", Path: "/api/v2/catalog/items/movie-tmdb-10378"}, reference, target)
	var known, unknown *conform.Difference
	for n := range report.Differences {
		switch report.Differences[n].Path {
		case "$/original_language":
			known = &report.Differences[n]
		case "$/not_a_contract_field":
			unknown = &report.Differences[n]
		}
	}
	if known == nil || known.Blocking || unknown == nil || !unknown.Blocking {
		t.Fatalf("schema classification: %+v", report.Differences)
	}
}
func TestObserveHLSAndRejectExternalURLs(t *testing.T) {
	p := &Protocol{}
	v := map[string]string{}
	start := conform.Request{Path: "/api/v2/playback/start"}
	p.Observe(start, conform.Response{Status: 201, Body: []byte(`{"outcome":"playable","session_id":"session","playback_plan":{"stream":{"url":"/api/v2/playback/transcode/stream.m3u8?st=secret"}}}`)}, v)
	if v["play_session_id"] != "session" || v["stream_token"] != "secret" {
		t.Fatal("playback response not propagated")
	}
	p.Observe(conform.Request{Path: "${stream_url}"}, conform.Response{Status: 200, Body: []byte("#EXTM3U\nsegment.ts?st=secret\n")}, v)
	if v["segment_url"] != "/api/v2/playback/transcode/segment.ts?st=secret" {
		t.Fatal("segment URL not resolved")
	}
	v = map[string]string{}
	p.Observe(start, conform.Response{Status: 201, Body: []byte(`{"outcome":"playable","playback_plan":{"stream":{"url":"https://attacker.test/playlist"}}}`)}, v)
	if v["stream_url"] != "" {
		t.Fatal("external stream URL accepted")
	}
}
func TestObserveExpandsHLSVariables(t *testing.T) {
	p := &Protocol{}
	vars := map[string]string{"stream_url": "/api/v2/playback/t/master.m3u8?st=secret"}
	body := []byte("#EXTM3U\n#EXT-X-DEFINE:NAME=\"silo_query\",VALUE=\"st=secret\"\nsegments/one.ts?{$silo_query}\n")
	p.Observe(conform.Request{Path: "${stream_url}"}, conform.Response{Status: 200, Body: body}, vars)
	if vars["segment_url"] != "/api/v2/playback/t/segments/one.ts?st=secret" || vars["stream_token"] != "secret" {
		t.Fatal("playlist variable was not resolved")
	}
	vars = map[string]string{"stream_url": "/a/list.m3u8"}
	p.Observe(conform.Request{Path: "${stream_url}"}, conform.Response{Status: 200, Body: []byte("#EXTM3U\nsegment.ts?{$missing}\n")}, vars)
	if vars["segment_url"] != "" {
		t.Fatal("undefined variable accepted")
	}
}

func TestFetchDescendsMasterBeforeObservingSegment(t *testing.T) {
	dir := t.TempDir()
	body := `{"protocol":"silo","client":"Silo","flows":{"play":[{"name":"stream","method":"GET","path":"${stream_url}"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "apple.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=900000\nlevels/low.m3u8\n"))
		case "/levels/low.m3u8":
			w.Write([]byte("#EXTM3U\n#EXTINF:4,\nsegment.ts\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	vars := map[string]string{"stream_url": "/master.m3u8"}
	request := p.Corpus("apple")[0]
	response, err := p.Fetch(context.Background(), server.Client(), server.URL, "", request, "apple", vars, true)
	if err != nil || vars["stream_url"] != "/levels/low.m3u8" {
		t.Fatalf("variant: %v %v", vars, err)
	}
	p.Observe(request, response, vars)
	if vars["segment_url"] != "/levels/segment.ts" {
		t.Fatalf("segment: %v", vars)
	}
	if _, _, err = masterVariant("/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhttps://example.com/evil.m3u8\n")); err == nil {
		t.Fatal("accepted cross-host variant")
	}
}
