package plex

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

var update = flag.Bool("update", false, "rewrite golden files")

// request performs a request against the fixture server.
// requestHook, when set (by the transcodechaos build), runs before every
// fixture request; it lets the chaos suites behave like real clients.
var requestHook func(f *fixture, method, target string, hdr map[string]string)

func (f *fixture) request(t *testing.T, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	if requestHook != nil {
		requestHook(f, method, target, hdr)
	}
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "192.168.1.50:51000"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if _, ok := hdr["X-Plex-Token"]; !ok && !strings.Contains(target, "X-Plex-Token") {
		req.Header.Set("X-Plex-Token", ownerToken)
	}
	if _, ok := hdr["X-Plex-Client-Identifier"]; !ok {
		req.Header.Set("X-Plex-Client-Identifier", "client-1")
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(t *testing.T, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	h := map[string]string{}
	for i := 0; i+1 < len(hdr); i += 2 {
		h[hdr[i]] = hdr[i+1]
	}
	return f.request(t, http.MethodGet, target, h)
}

// getWarm requests target once, lets the background enrichment it
// triggered (title details, UltraBlurColors) finish, then requests it
// again, so golden output doesn't depend on worker timing.
func (f *fixture) getWarm(t *testing.T, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	f.get(t, target, hdr...)
	f.srv.waitBackground()
	return f.get(t, target, hdr...)
}

var jsonAccept = []string{"Accept", "application/json"}

func checkGolden(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", name, rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	// Generation keys are opaque hashes of the retained row and request scope.
	// Compare their presence and placement, not the content-dependent digest.
	body = regexp.MustCompile(`etherShelf=[a-f0-9]{64}`).ReplaceAll(body, []byte("etherShelf=GENERATION"))
	// Stream and read tokens are per-start random capabilities; compare their
	// placement, not their value.
	body = regexp.MustCompile(`X-Ether-Stream-Token=[A-Z0-9]+`).ReplaceAll(body, []byte("X-Ether-Stream-Token=TOKEN"))
	body = regexp.MustCompile(`X-Ether-Read-Token=[A-Z0-9]+`).ReplaceAll(body, []byte("X-Ether-Read-Token=TOKEN"))
	ext := ".xml"
	ct := rec.Header().Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		ext = ".json"
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", "  "); err != nil {
			t.Fatalf("%s: invalid JSON: %v\n%s", name, err, body)
		}
		body = append(buf.Bytes(), '\n')
	case strings.HasPrefix(ct, "application/vnd.apple.mpegurl"), strings.HasPrefix(ct, "application/x-mpegurl"):
		// HLS playlists are plain text; compare verbatim.
		ext = ".m3u8"
	default:
		d := xml.NewDecoder(bytes.NewReader(body))
		for {
			_, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s: malformed XML: %v\n%s", name, err, body)
			}
		}
	}
	path := filepath.Join("testdata", "golden", name+ext)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: missing golden (run go test -update): %v", name, err)
	}
	if !bytes.Equal(want, body) {
		t.Errorf("%s: golden mismatch\n--- got ---\n%s\n--- want ---\n%s", name, body, want)
	}
}

func TestGolden(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, ws(bbE1.Key, 600_000, 0))
	f.state.set(ownerUser.ID, ws(alien.Key, 0, 2))

	appleTV := []string{"X-Plex-Product", "Plex for Apple TV", "X-Plex-Platform", "tvOS", "X-Plex-Client-Identifier", "atv-1"}
	cases := []struct {
		name   string
		target string
		hdr    []string
	}{
		{"identity", "/identity", nil},
		{"identity", "/identity", jsonAccept},
		{"root", "/", nil},
		{"media_providers", "/media/providers", jsonAccept},
		{"media_providers", "/media/providers", nil},
		{"sections", "/library/sections", nil},
		{"sections", "/library/sections", jsonAccept},
		{"section_1", "/library/sections/1?includeDetails=1", nil},
		{"section_1_all", "/library/sections/1/all?type=1&sort=titleSort&includeMeta=1&X-Plex-Container-Start=0&X-Plex-Container-Size=10", jsonAccept},
		{"section_1_all", "/library/sections/1/all?type=1&sort=titleSort&X-Plex-Container-Start=0&X-Plex-Container-Size=10", nil},
		{"section_1_genre", "/library/sections/1/genre?type=1", nil},
		{"section_2_all", "/library/sections/2/all", nil},
		{"metadata_movie", "/library/metadata/1000000000000550?includeExtras=1&checkFiles=1&asyncAugmentMetadata=1", nil},
		{"metadata_movie", "/library/metadata/1000000000000550?includeExtras=1", jsonAccept},
		{"metadata_show", "/library/metadata/2000000000001396?includeChildren=1&includeOnDeck=1", nil},
		{"children_show", "/library/metadata/2000000000001396/children", nil},
		{"children_season", "/library/metadata/3000000013960001/children", jsonAccept},
		{"allleaves_show", "/library/metadata/2000000000001396/allLeaves", nil},
		{"hubs", "/hubs?count=5", jsonAccept},
		{"hubs_section_1", "/hubs/sections/1", nil},
		{"hubs_continue", "/hubs/continueWatching", jsonAccept},
		{"hub_search", "/hubs/search?query=bay&limit=5", nil},
		{"library_search", "/library/search?query=fjord&searchTypes=movies", jsonAccept},
		{"collections_section_1", "/library/sections/1/collections", nil},
		{"collection_children", "/library/collections/5000000000008091/children", nil},
		{"on_deck", "/library/onDeck", nil},
		{"myplex_account", "/myplex/account", nil},
		{"myplex_account", "/myplex/account", jsonAccept},
		{"prefs", "/:/prefs", nil},
		{"servers", "/servers", nil},
		{"system_agents", "/system/agents", nil},
		{"decision_directplay", "/video/:/transcode/universal/decision?hasMDE=1&path=%2Flibrary%2Fmetadata%2F1000000000000550&mediaIndex=0&partIndex=0&protocol=hls&directPlay=1&directStream=1&session=s1", appleTV},
		{"decision_copy", "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1000000000000550&protocol=hls&directPlay=0&directStream=1&session=s2", jsonAccept},
	}
	for _, c := range cases {
		suffix := ""
		if len(c.hdr) >= 2 && c.hdr[1] == "application/json" {
			suffix = " json"
		}
		t.Run(c.name+suffix, func(t *testing.T) {
			checkGolden(t, c.name, f.getWarm(t, c.target, c.hdr...))
		})
	}
}

func TestGoldenStart(t *testing.T) {
	// A successful start must be served through the production wiring: the
	// encoder is reached via Host.Sessions (Prism), never Deps.Transcoder.
	enc := &recordedPlexEncoder{sessions: map[string]core.HLSSession{}}
	f := newFixture(withTestEncoder(enc))
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
	f.getWarm(t, decisionPath("/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=start-gold"))
	checkGolden(t, "start", f.getWarm(t, "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=start-gold"))
}

func TestGoldenDecisionNotAvailable(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.failures[fightClub.Key] = errWrap("resolve", errNotAvailable)
	checkGolden(t, "decision_not_available", f.getWarm(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&session=s3"))
	checkGolden(t, "decision_not_available", f.getWarm(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&session=s3", jsonAccept...))
}

func TestGoldenPlayQueue(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.getWarm(t, "/library/metadata/4000013960010001") // cache the show's detail
	rec := f.request(t, http.MethodPost, "/playQueues?type=video&uri=server%3A%2F%2F0123456789abcdef0123456789abcdef01234567%2Fcom.plexapp.plugins.library%2Flibrary%2Fmetadata%2F4000013960010001&continuous=1&repeat=0&own=1", map[string]string{"Accept": "application/json"})
	checkGolden(t, "playqueue_create", rec)
}

func TestGoldenSessions(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&hasMDE=1&directPlay=1&session=sess-1",
		"X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome", "X-Plex-Device", "OSX", "X-Plex-Device-Name", "Chrome", "X-Plex-Session-Identifier", "sess-1")
	if rec := f.get(t, "/library/parts/1000000000000550/1/file.mkv", "Range", "bytes=0-9", "X-Plex-Session-Identifier", "sess-1"); rec.Code != 206 {
		t.Fatalf("media delivery: %d", rec.Code)
	}
	f.get(t, "/:/timeline?ratingKey=1000000000000550&key=%2Flibrary%2Fmetadata%2F1000000000000550&state=playing&time=60000&duration=8340000",
		"X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome", "X-Plex-Device", "OSX", "X-Plex-Device-Name", "Chrome", "X-Plex-Session-Identifier", "sess-1")
	checkGolden(t, "status_sessions", f.getWarm(t, "/status/sessions"))
}
