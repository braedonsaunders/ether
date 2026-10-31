package plex

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
