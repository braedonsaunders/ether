package plex

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// #191, Plex for Android phones and tablets: embedded subtitle tracks (SRT and
// PGS) are listed on the pre-play screen but never reach the in-player
// subtitle panel. Unlike Plezy (mpv), the ExoPlayer-based Plex clients build
// that panel from the playback decision, not from the file. These tests replay
// an Android decision for a title with one embedded text track and one embedded
// image track in each delivery mode and assert that every track the player
// needs is present and shaped the way PMS answers it.
//
// The Android family is identified from the request (X-Plex-Product
// "Plex for Android", platform Android); it lists every probed track in a
// decision (the plex-list-all-tracks quirk). subtitles=none is the mode these
// apps send: "the player draws the track itself". That is true for an embedded
// text track the segments still carry, but it is not true for an image track
// the encoder has to burn, so the burn must survive the none mode (#191).

// androidEmbeddedInfo is the session's probed media: an embedded SRT and an
// embedded PGS beside the video and audio.
func androidEmbeddedInfo() core.MediaInfo {
	return core.MediaInfo{
		Container: "mkv", Duration: 1400 * time.Second, BitrateKbps: 8000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
			{Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "es"},
		},
	}
}

// androidSubsFixture is a probed title with the embedded SRT + PGS pair and a
// track preference that selects one language "always", so a decision names a
// selected embedded track without an explicit PUT.
func androidSubsFixture(t *testing.T, preferred string) *fixture {
	t.Helper()
	f, _ := newSubtitleFixture(t)
	f.playback.info = androidEmbeddedInfo()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	prefs := trackprefs.New(db)
	f.srv.setTrackPreferences(prefs)
	r := trackprefs.Default()
	r.SubtitleLanguages = []string{preferred}
	r.SubtitleMode = "always"
	if err := prefs.PutRules(context.Background(), ownerUser.ID, r); err != nil {
		t.Fatal(err)
	}
	return f
}

var androidPhone = []string{
	"X-Plex-Product", "Plex for Android",
	"X-Plex-Platform", "Android",
	"X-Plex-Device", "Pixel 8",
	"X-Plex-Client-Identifier", "android-191",
	"X-Plex-Client-Profile-Extra", "add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=aac,eac3&subtitleCodec=ass,ssa,pgs,srt)",
}

// androidDecision replays one Android playback decision. The fixture's real
// media-decision engine (openPlexPlan) chooses the decision, exactly as it does
// for a live request; a test only supplies the query the app sends. When dec is
// non-nil the fixture's transcoder returns it instead, which is how a test pins
// the output the real decider would produce (an image subtitle it burns).
func androidDecision(t *testing.T, f *fixture, name, query string, dec *core.Decision) string {
	t.Helper()
	if dec != nil {
		f.trans.decision = *dec
	}
	key := fightClub.Key.String()
	// Prime the pre-play detail page the app reads before it plays.
	if rec := f.get(t, "/library/metadata/"+key, androidPhone...); rec.Code != 200 {
		t.Fatalf("detail: %d", rec.Code)
	}
	rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&session=android-191-%s%s", key, name, query), androidPhone...)
	if rec.Code != 200 {
		t.Fatalf("decision %s: %d %s", name, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

var (
	androidSRTID = fmt.Sprintf("%d", slotHash(fightClub.Key, streamSlot(2, core.StreamInfo{Type: "subtitle", Codec: "subrip", Language: "en"})))
	androidPGSID = fmt.Sprintf("%d", slotHash(fightClub.Key, streamSlot(3, core.StreamInfo{Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "es"})))
)

// TestAndroidEmbeddedSubtitleIsListedInEveryMode pins the baseline the report
// describes as working online: the pre-play and decision stream lists both
// carry the embedded pair, with the id and codec the player maps by.
func TestAndroidEmbeddedSubtitleIsListedInEveryMode(t *testing.T) {
	modes := []struct {
		name  string
		query string
	}{
		{"directplay", "&directPlay=1&directStream=1&hasMDE=1&subtitles=none"},
		{"directstream", "&directPlay=1&directStream=1&hasMDE=1&subtitles=none"},
		{"transcode", "&directPlay=0&directStream=1&hasMDE=1&subtitles=none"},
	}
	for _, c := range modes {
		t.Run(c.name, func(t *testing.T) {
			f := androidSubsFixture(t, "en")
			body := androidDecision(t, f, c.name, c.query, nil)
			subs := subtitleTags(body)
			if len(subs) != 2 {
				t.Fatalf("%s: decision listed %d subtitle streams, want 2:\n%s", c.name, len(subs), body)
			}
			srt := tagWithID(subs, androidSRTID)
			pgs := tagWithID(subs, androidPGSID)
			if !strings.Contains(srt, `codec="srt"`) || streamAttr(selectedTag(subs), "id") != androidSRTID {
				t.Fatalf("%s: embedded SRT missing or not the selected track: %q", c.name, srt)
			}
			if !strings.Contains(pgs, `codec="pgs"`) {
				t.Fatalf("%s: embedded PGS missing from the decision: %q", c.name, pgs)
			}
		})
	}
}

// TestAndroidBurnedImageSubtitleReportsBurnUnderSubtitlesNone is the #191
// regression. Plex for Android sends subtitles=none with the same request that
// converts an embedded PGS, and the encoder burns the image into the picture.
// The decision must still mark that track decision="burn" so the player knows
// the cues are on screen; swallowing it under none left the player with no
// subtitle at all, in both a Matroska remux and an MPEG-TS transcode.
func TestAndroidBurnedImageSubtitleReportsBurnUnderSubtitlesNone(t *testing.T) {
	for _, c := range []struct {
		name  string
		query string
		dec   core.Decision
	}{
		{"directstream-mkv", "&directPlay=1&directStream=1&hasMDE=1&subtitles=none", core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, AudioCodec: "eac3", Container: "mkv", BurnSubtitles: true}},
		{"transcode-mpegts", "&directPlay=0&directStream=1&hasMDE=1&subtitles=none", core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts", BurnSubtitles: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := androidSubsFixture(t, "es") // selects the embedded PGS
			body := androidDecision(t, f, c.name, c.query, &c.dec)
			selected := selectedTag(subtitleTags(body))
			if !strings.Contains(selected, `codec="pgs"`) {
				t.Fatalf("%s: selected track is not the PGS: %q", c.name, selected)
			}
			if streamAttr(selected, "decision") != "burn" {
				t.Fatalf("%s: burned PGS reported %q, want burn:\n%s", c.name, streamAttr(selected, "decision"), body)
			}
			if streamAttr(selected, "location") == "" {
				t.Fatalf("%s: burn row names no delivery location: %q", c.name, selected)
			}
		})
	}
}

// TestAndroidSelectedEmbeddedSubtitleIsNeverDropped is the invariant the report
// asks for: whatever the delivery mode (none, auto, sidecar), the decision names
// the track the view selected instead of silently omitting it, so the in-player
// panel always has something to show.
func TestAndroidSelectedEmbeddedSubtitleIsNeverDropped(t *testing.T) {
	for _, mode := range []struct {
		name  string
		query string
	}{
		{"none", "&directPlay=1&directStream=1&hasMDE=1&subtitles=none"},
		{"auto", "&directPlay=1&directStream=1&hasMDE=1&subtitles=auto"},
		{"sidecar", "&directPlay=1&directStream=1&hasMDE=1&subtitles=sidecar&advancedSubtitles=text"},
	} {
		for _, lang := range []struct{ code, id string }{{"en", androidSRTID}, {"es", androidPGSID}} {
			t.Run(mode.name+"-"+lang.code, func(t *testing.T) {
				f := androidSubsFixture(t, lang.code)
				body := androidDecision(t, f, mode.name+"-"+lang.code, mode.query, nil)
				selected := selectedTag(subtitleTags(body))
				if streamAttr(selected, "id") != lang.id {
					t.Fatalf("%s/%s: selected %q, want track %s:\n%s", mode.name, lang.code, selected, lang.id, body)
				}
			})
		}
	}
}

func tagWithID(tags []string, id string) string {
	for _, tag := range tags {
		if streamAttr(tag, "id") == id {
			return tag
		}
	}
	return ""
}
