package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/conform"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompareDetectsStructureAndShape(t *testing.T) {
	cases := []struct{ name, a, b, kind string }{{"missing attr", `<MediaContainer size="1"><Video id="4"/></MediaContainer>`, `<MediaContainer size="1"><Video/></MediaContainer>`, `missing`}, {"type change", `<MediaContainer size="1"/>`, `<MediaContainer size="one"/>`, `type`}, {"extra element", `<MediaContainer/>`, `<MediaContainer><Video/></MediaContainer>`, `extra`}, {"changed ordering", `<MediaContainer><Directory/><Video/></MediaContainer>`, `<MediaContainer><Video/><Directory/></MediaContainer>`, `value`}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := conform.Compare(Response{Status: 200, Body: []byte(tc.a)}, Response{Status: 200, Body: []byte(tc.b)})
			found := false
			for _, d := range r.Differences {
				if d.Kind == tc.kind {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %s difference: %+v", tc.kind, r)
			}
		})
	}
}
func TestJSONComparisonAndVolatileShape(t *testing.T) {
	a := Response{Status: 200, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"MediaContainer":{"size":1,"machineIdentifier":"abc","Video":[{"id":4}]}}`)}
	b := a
	b.Body = []byte(`{"MediaContainer":{"size":"1","machineIdentifier":"xyz","Video":[{"id":8}],"extra":true}}`)
	r := conform.Compare(a, b)
	foundType, foundExtra := false, false
	for _, d := range r.Differences {
		foundType = foundType || d.Kind == "type"
		foundExtra = foundExtra || d.Kind == "extra"
	}
	if !foundType || !foundExtra {
		t.Fatalf("expected type and extra diffs: %+v", r)
	}
}
func TestRedact(t *testing.T) {
	s := string(conform.Redact([]byte(`X-Plex-Token=secret machineIdentifier="machine" user="alice" email=alice@example.com ip=192.168.1.4`)))
	for _, v := range []string{"secret", "machine\"", "alice@example.com", "192.168.1.4"} {
		if strings.Contains(s, v) {
			t.Errorf("unredacted %q in %s", v, s)
		}
	}
}

func TestTrimBodyAndPreserveRedactedJSON(t *testing.T) {
	var items strings.Builder
	items.WriteString(`<MediaContainer>`)
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&items, `<Video ratingKey="%d"/>`, i)
	}
	items.WriteString(`</MediaContainer>`)
	b, trimmed := conform.TrimBody([]byte(items.String()))
	if !trimmed {
		t.Fatal("expected XML listing to be trimmed")
	}
	n := len(strings.Split(string(b), "<Video ")) - 1
	if n != 50 {
		t.Fatalf("trimmed XML has %d entries", n)
	}
	var jsonItems strings.Builder
	jsonItems.WriteString(`{"MediaContainer":{"Metadata":[`)
	for i := 0; i < 60; i++ {
		if i > 0 {
			jsonItems.WriteByte(',')
		}
		fmt.Fprintf(&jsonItems, `{"ratingKey":%d}`, i)
	}
	jsonItems.WriteString(`]}}`)
	jsonBody, jsonTrimmed := conform.TrimBody([]byte(jsonItems.String()))
	if !jsonTrimmed {
		t.Fatal("expected JSON listing to be trimmed")
	}
	var jsonDoc map[string]any
	if e := json.Unmarshal(jsonBody, &jsonDoc); e != nil {
		t.Fatal(e)
	}
	got := jsonDoc["MediaContainer"].(map[string]any)["Metadata"].([]any)
	if len(got) != 50 {
		t.Fatalf("trimmed JSON has %d entries", len(got))
	}
	secret := `{"machineIdentifier":"machine-secret","friendlyName":"secret server","username":"alice","X-Plex-Token":"token-secret","email":"alice@example.com","address":"192.168.1.30"}`
	redacted := conform.Redact([]byte(secret))
	var parsed map[string]any
	if e := json.Unmarshal(redacted, &parsed); e != nil {
		t.Fatalf("redaction broke JSON: %s (%v)", redacted, e)
	}
	for _, s := range []string{"machine-secret", "secret server", "alice", "token-secret", "alice@example.com", "192.168.1.30"} {
		if strings.Contains(string(redacted), s) {
			t.Errorf("leaked %q in %s", s, redacted)
		}
	}
}
func TestCorpusFetchesSameTemplatedIDsForServers(t *testing.T) {
	var paths []string
	makeServer := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/xml")
			if r.URL.Path == "/library/sections" {
				w.Write([]byte(`<MediaContainer><Directory key="` + id + `"/></MediaContainer>`))
			} else {
				w.Write([]byte(`<MediaContainer/>`))
			}
		}))
	}
	a, b := makeServer("3"), makeServer("7")
	defer a.Close()
	defer b.Close()
	ctx := context.Background()
	req := Corpus()[3]
	for _, srv := range []*httptest.Server{a, b} {
		if _, e := Fetch(ctx, srv.Client(), srv.URL, "", req, "web", map[string]string{"section": "3", "ratingKey": "42"}, false); e != nil {
			t.Fatal(e)
		}
	}
	if len(paths) != 2 || paths[0] != paths[1] || !strings.Contains(paths[0], "/3/all") {
		t.Fatalf("request paths not consistent: %v", paths)
	}
}

func TestDiscoverAlignsSameTitlesAcrossDifferentServerIDs(t *testing.T) {
	server := func(movieFirst, movieShared, showFirst, showShared, season, episode string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			switch {
			case r.URL.Path == "/library/sections":
				fmt.Fprint(w, `<MediaContainer><Directory key="1" type="movie"/><Directory key="2" type="show"/></MediaContainer>`)
			case strings.HasSuffix(r.URL.Path, "/all") && strings.Contains(r.URL.Path, "/1/"):
				firstMovieGuid, sharedMovieGuid := "tmdb://999", "tmdb://111"
				if movieFirst == "99" {
					firstMovieGuid, sharedMovieGuid = "tmdb://000", "tmdb://999"
				}
				fmt.Fprintf(w, `<MediaContainer><Video ratingKey="%s"><Guid id="%s"/></Video><Video ratingKey="%s"><Guid id="%s"/></Video></MediaContainer>`, movieFirst, firstMovieGuid, movieShared, sharedMovieGuid)
			case strings.HasSuffix(r.URL.Path, "/all"):
				firstShowGuid, sharedShowGuid := "imdb://tt0000001", "tmdb://222"
				if showFirst == "98" {
					firstShowGuid, sharedShowGuid = "tmdb://000", "imdb://tt0000001"
				}
				fmt.Fprintf(w, `<MediaContainer><Directory ratingKey="%s"><Guid id="%s"/></Directory><Directory ratingKey="%s"><Guid id="%s"/></Directory></MediaContainer>`, showFirst, firstShowGuid, showShared, sharedShowGuid)
			case (r.URL.Path == "/library/metadata/"+showFirst+"/children" || r.URL.Path == "/library/metadata/"+showShared+"/children"):
				fmt.Fprintf(w, `<MediaContainer><Directory ratingKey="%s" index="1"/></MediaContainer>`, season)
			case r.URL.Path == "/library/metadata/"+season+"/children":
				fmt.Fprintf(w, `<MediaContainer><Video ratingKey="%s" index="1"/></MediaContainer>`, episode)
			case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
				fmt.Fprintf(w, `<MediaContainer><Video ratingKey="%s" index="1"/></MediaContainer>`, episode)
			case r.URL.Path == "/library/collections":
				fmt.Fprint(w, `<MediaContainer><Directory ratingKey="88"/></MediaContainer>`)
			case r.URL.Path == "/playlists":
				fmt.Fprint(w, `<MediaContainer><Playlist ratingKey="77"/></MediaContainer>`)
			default:
				fmt.Fprint(w, `<MediaContainer/>`)
			}
		}))
	}
	a := server("10", "11", "20", "21", "30", "31")
	b := server("99", "12", "98", "22", "40", "41")
	defer a.Close()
	defer b.Close()
	ctx := context.Background()
	left := Discover(ctx, a.Client(), a.URL, "", "web", nil)
	right := Discover(ctx, b.Client(), b.URL, "", "web", &left)
	for k, want := range map[string]string{"movieKey": "12", "showKey": "22", "seasonKey": "40", "episodeKey": "41", "collectionKey": "88", "playlistKey": "77"} {
		if right.Values[k] != want {
			t.Errorf("%s = %q, want %q (left=%v right=%v)", k, right.Values[k], want, left.Values, right.Values)
		}
	}
}
func TestPlexProtocolHeaderIsCompared(t *testing.T) {
	a := Response{Status: 200, Headers: http.Header{"X-Plex-Protocol": []string{"1.0"}}, Body: []byte(`{}`)}
	b := a
	b.Headers = http.Header{}
	if Compare(a, b).Blocking() != 1 {
		t.Fatal("missing Plex protocol header was not reported")
	}
	if conform.Compare(a, b).Blocking() != 0 {
		t.Fatal("generic comparison included a Plex-specific header")
	}
}
