package plex

import (
	"regexp"
	"strings"
	"testing"
)

// TestServerMediaURLsOmitAccountToken walks the media URLs the server
// writes into metadata, a decision, and HLS and DASH playlists. Plex
// clients append their own X-Plex-Token. The server must not embed one.
func TestServerMediaURLsOmitAccountToken(t *testing.T) {
	f, _ := dashFixture()
	defer f.srv.Close()
	key := fightClub.Key.String()
	pages := []string{
		"/library/metadata/" + key,
		"/video/:/transcode/universal/decision?path=/library/metadata/" + key + "&session=tok-dec&protocol=hls&directPlay=0&directStream=1&hasMDE=1",
		"/video/:/transcode/universal/start.m3u8?path=/library/metadata/" + key + "&session=tok-hls&protocol=hls&directPlay=0&directStream=1",
		"/video/:/transcode/universal/start.mpd?path=/library/metadata/" + key + "&session=tok-dash&protocol=dash&directPlay=0&directStream=1",
		"/photo/:/transcode?width=300&height=450&minSize=1&upscale=1&url=" + urlEsc("/library/metadata/"+key+"/thumb/1759276800"),
	}
	urlPattern := regexp.MustCompile(`(?:https?:)?/[^"'\s<>]+`)
	seen := map[string]bool{}
	var pending []string
	check := func(body string) {
		t.Helper()
		if strings.Contains(strings.ToLower(body), "x-plex-token") || strings.Contains(body, ownerToken) {
			t.Fatalf("account token embedded in server media:\n%s", body)
		}
		for _, match := range urlPattern.FindAllString(body, -1) {
			if seen[match] || !strings.Contains(match, "/") {
				continue
			}
			seen[match] = true
			if strings.HasPrefix(match, "/") && (strings.Contains(match, "/session/") || strings.Contains(match, "/library/parts/") || strings.Contains(match, "trickplay") || strings.Contains(match, "/subtitles")) {
				pending = append(pending, match)
			}
		}
	}
	for _, path := range pages {
		rec := f.get(t, path)
		if rec.Code != 200 && rec.Code != 206 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		check(rec.Body.String())
	}
	for len(pending) > 0 {
		path := pending[0]
		pending = pending[1:]
		rec := f.get(t, path)
		check(rec.Body.String())
	}
}
