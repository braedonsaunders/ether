package plex

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Plex for Android builds pre-play selectors from the selected Media's
// streams and the in-player menu from the decision. Before a probe the 4K
// version has no streams. After Stop the probe must stay on that version,
// a PUT must stick, and a remux decision must still list the other audio.
func TestAndroidTrackChoiceSurvivesStop(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	pb.info = probedInfo
	android := []string{"X-Plex-Product", "Plex for Android", "X-Plex-Platform", "Android"}
	key := fightClub.Key.String()

	before := f.get(t, "/library/metadata/"+key, android...).Body.String()
	if block := mediaBlock(before, "4K Dolby Vision Remux"); block == "" || strings.Contains(block, "<Stream ") {
		t.Fatalf("pre-probe 4K version should expose no streams yet:\n%s", block)
	}

	decision := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+key+"&mediaIndex=1&directPlay=0&directStream=1&protocol=hls&session=and1", android...)
	if decision.Code != http.StatusOK {
		t.Fatalf("decision %d: %s", decision.Code, decision.Body.String())
	}
	assertRemuxAudio(t, decision.Body.String(), "eng", "fre")

	pb.mu.Lock()
	clear(pb.sessions)
	pb.mu.Unlock()

	after := f.get(t, "/library/metadata/"+key, android...)
	if after.Code != http.StatusOK {
		t.Fatalf("metadata after stop %d", after.Code)
	}
	block := mediaBlock(after.Body.String(), "4K Dolby Vision Remux")
	audios := audioTags(block)
	if len(audios) < 2 || !strings.Contains(block, `selected="1"`) {
		t.Fatalf("4K version lost its probed tracks after stop: %q\n%s", audios, block)
	}
	if compat := mediaBlock(after.Body.String(), "720p Blu-ray (High Compatibility)"); strings.Contains(compat, `languageCode="fre"`) || strings.Contains(compat, "<Stream ") {
		t.Fatalf("another version inherited the resumed tracks:\n%s", compat)
	}
	friend := friendMetadata(t, f, "/library/metadata/"+key)
	if strings.Contains(friend, `languageCode="fre"`) {
		t.Fatal("another viewer received the owner's audio tracks")
	}
	if other := mediaBlock(friend, "4K Dolby Vision Remux"); strings.Contains(other, "<Stream ") {
		t.Fatalf("another viewer received the owner's 4K probe:\n%s", other)
	}
	french := streamID(audioByLang(audios, "fre"))
	if french == "" {
		t.Fatalf("french audio id missing: %q", audios)
	}
	part := variantPartRE.FindStringSubmatch(block)
	if part == nil {
		t.Fatalf("4K part key missing:\n%s", block)
	}

	put := httptest.NewRequest(http.MethodPut, "/library/parts/"+part[1]+"?audioStreamID="+french+"&allParts=1", nil)
	put.RemoteAddr = "192.168.1.50:51000"
	put.Header.Set("X-Plex-Token", ownerToken)
	put.Header.Set("X-Plex-Client-Identifier", "client-1")
	put.Header.Set("X-Plex-Product", "Plex for Android")
	put.Header.Set("X-Plex-Platform", "Android")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("select french audio: %d %s", rec.Code, rec.Body.String())
	}

	again := mediaBlock(f.get(t, "/library/metadata/"+key, android...).Body.String(), "4K Dolby Vision Remux")
	chosen := audioByLang(audioTags(again), "fre")
	if !strings.Contains(again, `title="4K Dolby Vision Remux"`) || !strings.Contains(chosen, `selected="1"`) || len(audioTags(again)) < 2 {
		t.Fatalf("selection did not stick on the 4K version:\n%s", again)
	}

	replay := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+key+"&mediaIndex=1&directPlay=0&directStream=1&protocol=hls&session=and2&audioStreamID="+french, android...)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay decision %d: %s", replay.Code, replay.Body.String())
	}
	assertRemuxAudio(t, replay.Body.String(), "fre", "eng")
}

func TestDecisionListsAlternateTracksForAndroidOnly(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	path := "/library/metadata/" + fightClub.Key.String()
	clients := []struct {
		name     string
		product  string
		platform string
		device   string
		android  bool
	}{
		{"web", "Plex Web", "Chrome", "", false},
		{"web reporting android", "Plex Web", "Android", "", false},
		{"ios", "Plex for iOS", "iOS", "", false},
		{"apple tv", "Plex for Apple TV", "tvOS", "", false},
		{"android", "Plex for Android", "Android", "", true},
		{"android tv", "Plex for Android (TV)", "Android", "SHIELD Android TV", true},
		{"fire tv", "Plex for Android", "Android", "AFTKA", true},
	}
	for _, client := range clients {
		t.Run(client.name, func(t *testing.T) {
			hdr := []string{"X-Plex-Product", client.product, "X-Plex-Platform", client.platform}
			if client.device != "" {
				hdr = append(hdr, "X-Plex-Device", client.device)
			}
			f.trans.decision = core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mkv"}
			slug := strings.ReplaceAll(client.name, " ", "-")
			direct := f.get(t, "/video/:/transcode/universal/decision?path="+path+"&protocol=hls&directPlay=1&directStream=1&session=alt-direct-"+slug, hdr...)
			if direct.Code != http.StatusOK {
				t.Fatalf("direct decision %d: %s", direct.Code, direct.Body.String())
			}
			assertAlternateTracks(t, direct.Body.String(), client.android, true)

			f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mkv"}
			remux := f.get(t, "/video/:/transcode/universal/decision?path="+path+"&protocol=hls&directPlay=0&directStream=1&session=alt-remux-"+slug, hdr...)
			if remux.Code != http.StatusOK {
				t.Fatalf("remux decision %d: %s", remux.Code, remux.Body.String())
			}
			assertAlternateTracks(t, remux.Body.String(), client.android, false)
		})
	}
}

func assertAlternateTracks(t *testing.T, body string, android, direct bool) {
	t.Helper()
	audios := audioTags(body)
	subs := streamTagsOfType(body, "3")
	if !android {
		if len(audios) != 1 || len(subs) != 0 || strings.Contains(body, `languageCode="fre"`) {
			t.Fatalf("non-Android decision listed alternate tracks:\n%s", body)
		}
		return
	}
	if len(audios) < 2 || audioByLang(audios, "fre") == "" || len(subs) < 2 {
		t.Fatalf("Android decision hid alternate tracks: audio=%q subs=%d\n%s", audios, len(subs), body)
	}
	other := audioByLang(audios, "fre")
	if direct {
		if !strings.Contains(other, `location="direct"`) {
			t.Fatalf("Android direct-play alternate: %s", other)
		}
		return
	}
	if strings.Contains(other, `decision="`) || !strings.Contains(other, `index="`) {
		t.Fatalf("Android remux alternate: %s", other)
	}
}

func streamTagsOfType(body, streamType string) []string {
	var out []string
	for _, tag := range streamTagRe.FindAllString(body, -1) {
		if strings.Contains(tag, `streamType="`+streamType+`"`) {
			out = append(out, tag)
		}
	}
	return out
}

func friendMetadata(t *testing.T, f *variantFixture, target string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "192.168.1.51:51000"
	req.Header.Set("X-Plex-Token", friendToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-2")
	req.Header.Set("X-Plex-Product", "Plex for Android")
	req.Header.Set("X-Plex-Platform", "Android")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("friend metadata %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func assertRemuxAudio(t *testing.T, body, selected, alternate string) {
	t.Helper()
	audios := audioTags(body)
	if len(audios) < 2 {
		t.Fatalf("decision hid alternate audio: %q", audios)
	}
	gotSelected := audioByLang(audios, selected)
	gotOther := audioByLang(audios, alternate)
	if !strings.Contains(gotSelected, `selected="1"`) || !strings.Contains(gotSelected, `decision="copy"`) {
		t.Fatalf("selected %s audio: %s", selected, gotSelected)
	}
	if gotOther == "" || strings.Contains(gotOther, `decision="`) {
		t.Fatalf("alternate %s audio should stay listed and out of the remux: %s", alternate, gotOther)
	}
}

func mediaBlock(body, title string) string {
	for _, part := range strings.Split(body, "<Media ")[1:] {
		chunk := "<Media " + part
		if end := strings.Index(chunk, "</Media>"); end >= 0 {
			chunk = chunk[:end]
		}
		if strings.Contains(chunk, `title="`+title+`"`) {
			return chunk
		}
	}
	return ""
}

func audioTags(body string) []string {
	var out []string
	for _, tag := range streamTagRe.FindAllString(body, -1) {
		if strings.Contains(tag, `streamType="2"`) {
			out = append(out, tag)
		}
	}
	return out
}

func audioByLang(tags []string, code string) string {
	for _, tag := range tags {
		if strings.Contains(tag, `languageCode="`+code+`"`) {
			return tag
		}
	}
	return ""
}

var streamIDRE = regexp.MustCompile(`id="(\d+)"`)

func streamID(tag string) string {
	m := streamIDRE.FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return m[1]
}
