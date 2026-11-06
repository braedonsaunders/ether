package plex

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// Exercise the decision AND start handoff with the Android TV request shape.
// PMS 1.42.2's subtitles_s1422 capture supplies the conversion contract:
// mkv target, selected text copy/format/segments-av, no extraction key.
// The Shield's recorded Original-quality request supplies hasMDE/subtitles=none.
func TestShieldSubtitleDeliveryContract(t *testing.T) {
	for _, product := range []string{"Plex for Android (TV)", "Plex for Android (Mobile)"} {
		t.Run(product, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			f.srv.d.Transcoder = shieldContractTranscoder{f.trans}
			f.playback.info = core.MediaInfo{Container: "mkv", Duration: 30 * time.Second, BitrateKbps: 1000, Streams: []core.StreamInfo{
				{Index: 0, Type: "video", Codec: "h264", Width: 640, Height: 360},
				{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Default: true},
				{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en", Default: true},
				{Index: 3, Type: "subtitle", Codec: "ass", Language: "de"},
				{Index: 4, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "fr", Forced: true},
			}}
			device := "SHIELD Android TV"
			if product == "Plex for Android (Mobile)" {
				device = "Pixel 8"
			}
			hdr := []string{"X-Plex-Product", product, "X-Plex-Platform", "Android", "X-Plex-Device", device, "X-Plex-Model", "darcy"}
			profile := "add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264,hevc,vp8,vp9&audioCodec=aac,pcm,mp3,ac3,dca,eac3,truehd&subtitleCodec=ass,ssa,pgs,srt)"
			key := fightClub.Key.String()
			tags := subtitleTags(f.get(t, "/library/metadata/"+key, hdr...).Body.String())
			for _, c := range []struct {
				lang  string
				index int
			}{{"eng", 2}, {"ger", 3}, {"fre", 4}} {
				id := ""
				for _, tag := range tags {
					if streamAttr(tag, "index") == strconv.Itoa(c.index) {
						id = streamAttr(tag, "id")
						if (c.index == 2 && streamAttr(tag, "default") != "1") || (c.index == 4 && streamAttr(tag, "forced") != "1") {
							t.Fatalf("track flags: %s", tag)
						}
						if streamAttr(tag, "key") != "" || streamAttr(tag, "languageTag") == "" || streamAttr(tag, "codec") == "" {
							t.Fatalf("embedded metadata: %s", tag)
						}
					}
				}
				if id == "" {
					t.Fatalf("missing track %d: %v", c.index, tags)
				}
				for _, mode := range []string{"direct", "remux", "transcode", "burn"} {
					t.Run(c.lang+"/"+mode, func(t *testing.T) {
						q := url.Values{"path": {"/library/metadata/" + key}, "protocol": {"hls"}, "directPlay": {"1"}, "directStream": {"1"}, "hasMDE": {"1"}, "subtitles": {"none"}, "advancedSubtitles": {"text"}, "session": {"shield-" + mode}, "subtitleStreamID": {id}, "X-Plex-Client-Profile-Name": {"Generic"}, "X-Plex-Client-Profile-Extra": {profile}}
						if mode == "burn" {
							q.Set("subtitles", "burn")
						}
						if mode == "transcode" {
							q.Set("maxVideoBitrate", "100")
							q.Set("videoResolution", "320x180")
						}
						if mode != "direct" && mode != "burn" {
							q.Set("directPlay", "0")
						}
						rec := f.get(t, "/video/:/transcode/universal/decision?"+q.Encode(), hdr...)
						if rec.Code != http.StatusOK {
							t.Fatalf("decision: %d %s", rec.Code, rec.Body.String())
						}
						row := selectedTag(subtitleTags(rec.Body.String()))
						if streamAttr(row, "id") != id {
							t.Fatalf("selected row: %s", row)
						}
						if mode == "direct" && !(product == "Plex for Android (Mobile)" && c.index == 4) && (streamAttr(row, "decision") != "ignore" || streamAttr(row, "index") != strconv.Itoa(c.index) || streamAttr(row, "key") != "") {
							t.Fatalf("native track contract: %s", row)
						}
						if (mode != "direct" || product == "Plex for Android (Mobile)") && c.index == 4 && (streamAttr(row, "decision") != "burn" || streamAttr(row, "location") != "segments-av" || streamAttr(row, "burn") != "1") {
							t.Fatalf("PGS burn contract: %s", row)
						}
						rec = f.get(t, "/video/:/transcode/universal/start.m3u8?"+q.Encode(), hdr...)
						if rec.Code != http.StatusOK {
							t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
						}
						hs := f.trans.sessions[q.Get("session")]
						if hs == nil {
							t.Fatal("no encoder")
						}
						d := hs.req.Decision
						if (c.index == 4 || mode == "burn") && (!d.BurnSubtitles || d.VideoCopy) {
							t.Errorf("selected track was not burned: %+v", d)
						}
						if d.Container != transcode.ContainerMatroska || d.SubtitleStream != c.index {
							t.Errorf("HLS handoff lost selected track/target: %+v", d)
						}
						if mode != "direct" && mode != "burn" && c.index != 4 && (!strings.Contains(row, `location="segments-av"`) || streamAttr(row, "format") == "" || streamAttr(row, "key") != "") {
							t.Errorf("conversion row: %s", row)
						}
						// Reuse the running session, as the in-player picker does.
						q.Set("subtitleStreamID", "0")
						q.Set("subtitles", "none")
						off := f.get(t, "/video/:/transcode/universal/start.m3u8?"+q.Encode(), hdr...)
						if off.Code != http.StatusOK {
							t.Fatalf("subtitles off: %d", off.Code)
						}
						if d := f.trans.sessions[q.Get("session")].req.Decision; d.SubtitleStream != -1 || d.BurnSubtitles {
							t.Errorf("off retained a subtitle: %+v", d)
						}

					})
				}
			}
		})
	}
}

// Use the production decision engine; the fake only records encoder handoff.
type shieldContractTranscoder struct{ *fakeTranscoder }

func (s shieldContractTranscoder) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return transcode.Decide(info, caps, opts, transcode.Policy{})
}
