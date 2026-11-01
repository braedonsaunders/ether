package plex

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// extractTranscoder adds embedded-subtitle extraction to the fake.
type extractTranscoder struct {
	*fakeTranscoder
	out  string
	fail error
}

func (e *extractTranscoder) ExtractSubtitle(_ context.Context, _ string, _ int, _ string, w io.Writer) error {
	if e.fail != nil {
		return e.fail
	}
	_, err := io.WriteString(w, e.out)
	return err
}

const sampleVTT = "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nHello world\n"

// A subtitle picked from an embedded track during a transcode session is
// extracted and served (offset-shifted), instead of an empty container.
func TestEmbeddedSubtitleServedFromSession(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	const vtt = sampleVTT
	f.srv.d.Transcoder = &extractTranscoder{fakeTranscoder: f.trans, out: vtt}
	f.playback.info = probedInfo

	key := fightClub.Key.String()
	// Start a remux session so a transcode entry exists, then ask for the
	// embedded subtitle through the session endpoint.
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+key+"&directPlay=0&directStream=1&session=embsub1&protocol=hls",
		"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android",
		"X-Plex-Client-Identifier", "client-1")
	if rec.Code != 200 {
		t.Fatalf("decision %d: %s", rec.Code, rec.Body.String())
	}
	// Embedded (French, index 3) subtitle id as rendered in metadata.
	f.get(t, "/library/metadata/"+key)
	embedded := core.StreamInfo{Index: 3, Type: "subtitle", Codec: "subrip", Language: "fr"}
	subID := slotHash(fightClub.Key, streamSlot(3, embedded))
	rec = f.get(t, fmt.Sprintf("/video/:/transcode/universal/subtitles?path=/library/metadata/%s&session=embsub1&subtitleStreamID=%d&format=vtt", key, subID),
		"X-Plex-Client-Identifier", "client-1")
	body := rec.Body.String()
	if body == "" || body == "<MediaContainer size=\"0\"/>" || !contains(body, "Hello world") {
		t.Fatalf("embedded subtitle not served: %q", body)
	}

	// Selecting the embedded subtitle in a remux decision must report it as
	// location="sidecar" with key="/library/streams/{id}" (since FFmpeg runs
	// with -sn) and serve extracted cues from /library/streams/{id}.
	decSub := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&directPlay=0&directStream=1&session=embsub2&protocol=hls&subtitleStreamID=%d", key, subID),
		"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android",
		"X-Plex-Client-Identifier", "client-1")
	if decSub.Code != 200 {
		t.Fatalf("decision with selected sub %d: %s", decSub.Code, decSub.Body.String())
	}
	streamKey := fmt.Sprintf("/library/streams/%d", subID)
	if !strings.Contains(decSub.Body.String(), `location="sidecar"`) || !strings.Contains(decSub.Body.String(), `key="`+streamKey+`"`) {
		t.Fatalf("selected embedded subtitle in remux decision missing sidecar key %s:\n%s", streamKey, decSub.Body.String())
	}
	streamRec := f.get(t, streamKey+"?format=vtt")
	if streamRec.Code != 200 || !contains(streamRec.Body.String(), "Hello world") {
		t.Fatalf("GET %s returned %d: %q", streamKey, streamRec.Code, streamRec.Body.String())
	}
}

// TestDirectPlayEmbeddedTextIsSidecar is Plex Web and Plex for Android
// selecting an embedded SRT or ASS during direct play. The decision must
// name a sidecar URL, and that URL must return the cues. Bitmap PGS is
// burned into the picture instead.
func TestDirectPlayEmbeddedTextIsSidecar(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	const vtt = sampleVTT
	f.srv.d.Transcoder = &extractTranscoder{fakeTranscoder: f.trans, out: vtt}
	f.trans.decision = core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mkv"}
	f.playback.info = core.MediaInfo{
		Container: "mkv", Duration: 14 * time.Minute, BitrateKbps: 4000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 800, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
			{Index: 3, Type: "subtitle", Codec: "ass", Language: "fr"},
			{Index: 4, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "es"},
		},
	}
	key := fightClub.Key
	meta := f.get(t, "/library/metadata/"+key.String())
	tags := subtitleTags(meta.Body.String())
	srtID := subtitleChoice(tags, "eng", false)
	assID := subtitleChoice(tags, "fre", false)
	pgsID := ""
	for _, tag := range tags {
		if strings.Contains(tag, `codec="pgs"`) {
			pgsID = streamAttr(tag, "id")
		}
	}
	if srtID == "" || assID == "" || pgsID == "" {
		t.Fatalf("metadata subtitle ids srt=%q ass=%q pgs=%q tags=%q", srtID, assID, pgsID, tags)
	}

	clients := []struct {
		name, protocol string
		hdr            []string
	}{
		{"web", "dash", []string{"X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome", "X-Plex-Client-Identifier", "web-subs"}},
		{"android", "hls", []string{"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android", "X-Plex-Client-Identifier", "android-subs"}},
	}
	for _, c := range clients {
		for _, id := range []string{srtID, assID} {
			rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&protocol=%s&directPlay=1&directStream=1&session=%s-%s&subtitleStreamID=%s", key, c.protocol, c.name, id, id), c.hdr...)
			if rec.Code != 200 {
				t.Fatalf("%s decision %d: %s", c.name, rec.Code, rec.Body.String())
			}
			selected := selectedTag(subtitleTags(rec.Body.String()))
			if !strings.Contains(selected, `location="sidecar"`) || !strings.Contains(selected, `key="/library/streams/`) || strings.Contains(selected, `location="direct"`) {
				t.Fatalf("%s embedded text decision: %s", c.name, selected)
			}
			streamKey := streamAttr(selected, "key")
			got := f.get(t, streamKey, c.hdr...)
			if got.Code != 200 || !contains(got.Body.String(), "Hello world") {
				t.Fatalf("%s GET %s: %d %q", c.name, streamKey, got.Code, got.Body.String())
			}
		}
	}

	f.trans.decision = core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts", BurnSubtitles: true}
	rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&protocol=dash&directPlay=0&directStream=1&session=web-pgs&subtitleStreamID=%s", key, pgsID),
		"X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome", "X-Plex-Client-Identifier", "web-subs")
	selected := selectedTag(subtitleTags(rec.Body.String()))
	if !strings.Contains(selected, `decision="burn"`) || !strings.Contains(selected, `location="segments-video"`) {
		t.Fatalf("web PGS burn decision: %s\n%s", selected, rec.Body.String())
	}
	rec = f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&protocol=hls&directPlay=0&directStream=1&session=and-pgs&subtitleStreamID=%s", key, pgsID),
		"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android", "X-Plex-Client-Identifier", "android-subs")
	selected = selectedTag(subtitleTags(rec.Body.String()))
	if !strings.Contains(selected, `decision="burn"`) || !strings.Contains(selected, `location="segments-video"`) {
		t.Fatalf("android PGS burn decision: %s\n%s", selected, rec.Body.String())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
