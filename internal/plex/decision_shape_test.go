package plex

import (
	"regexp"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func coreDecision(kind string) core.Decision {
	switch kind {
	case "directplay":
		return core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mkv"}
	case "remux":
		return core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mkv"}
	default:
		return core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts"}
	}
}

// For the Android client, what does /video/:/transcode/universal/decision
// send on unselected audio/subtitle streams? Both shapes currently under
// discussion keep the streams in the list:
//   - dev branch (bare): index kept, no decision/location attributes
//     (same shape PMS uses for streams in library metadata)
//   - local fix (shaped): decision copy/transcode + segments-av /
//     segments-subtitle / sidecar location, like the selected stream
//
// This test pins the wire bytes so a real PMS capture can be diffed
// attribute-by-attribute instead of argued from memory.
func TestAndroidDecisionListsAllTracks(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	hdr := []string{
		"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android",
		"X-Plex-Client-Identifier", "android1",
		"X-Plex-Device", "S24Plus", "X-Plex-Provides", "player",
	}
	path := "/video/:/transcode/universal/decision?path=/library/metadata/" + fightClub.Key.String() +
		"&mediaIndex=0&partIndex=0&protocol=hls&fastSeek=1&directPlay=1&directStream=1&session=shape1"

	for _, kind := range []struct {
		name string
		set  func(*fakeTranscoder)
	}{
		{"directplay", func(tr *fakeTranscoder) {
			tr.decision = coreDecision("directplay")
		}},
		{"remux", func(tr *fakeTranscoder) {
			tr.decision = coreDecision("remux")
		}},
		{"transcode", func(tr *fakeTranscoder) {
			tr.decision = coreDecision("transcode")
		}},
	} {
		t.Run(kind.name, func(t *testing.T) {
			kind.set(f.trans)
			rec := f.get(t, path, hdr...)
			if rec.Code != 200 {
				t.Fatalf("decision %d: %s", rec.Code, rec.Body.String())
			}
			streams := streamTags(rec.Body.String())
			audio, subs, selected := 0, 0, 0
			for _, st := range streams {
				switch {
				case strings.Contains(st, `streamType="2"`):
					audio++
				case strings.Contains(st, `streamType="3"`):
					subs++
				}
				if strings.Contains(st, `selected="1"`) {
					selected++
				}
				t.Logf("%s stream: %s", kind.name, st)
			}
			if audio != 2 || subs != 2 {
				t.Errorf("%s: audio=%d subs=%d, want 2/2 (alternates missing)", kind.name, audio, subs)
			}
			if selected != 1 {
				t.Errorf("%s: selected=%d, want exactly 1", kind.name, selected)
			}
		})
	}
}

var streamTagRE = regexp.MustCompile(`<Stream [^>]*/>`)

func streamTags(xml string) []string { return streamTagRE.FindAllString(xml, -1) }
