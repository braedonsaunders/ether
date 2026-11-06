package plex

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPMSMediaGeometryAndAudioSummary(t *testing.T) {
	for _, tc := range []struct {
		w, h       int
		resolution string
	}{
		{1920, 1600, "2.7k"}, {1920, 2160, "2.7k"}, {2048, 858, "2k"}, {2560, 1376, "2k"},
		{1440, 1048, "720"}, {1852, 800, "720"}, {1920, 800, "1080"}, {856, 358, "480"}, {640, 360, "sd"},
	} {
		f := newFixture()
		f.playback.info.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
		f.playback.info.Streams[0].Width, f.playback.info.Streams[0].Height = tc.w, tc.h
		f.playback.info.Streams = append(f.playback.info.Streams, core.StreamInfo{Index: 8, Type: "audio", Codec: "truehd", Channels: 8}, core.StreamInfo{Index: 9, Type: "audio", Codec: "aac", Channels: 8})
		r := f.get(t, "/library/metadata/1000000000000550", jsonAccept...)
		f.srv.Close()
		if r.Code != 200 {
			t.Fatalf("metadata %d %s", r.Code, r.Body.String())
		}
		m := decodeJSON(t, r)["Metadata"].([]any)[0].(map[string]any)["Media"].([]any)[0].(map[string]any)
		if m["videoResolution"] != tc.resolution || m["audioCodec"] != "truehd" || m["audioChannels"] != float64(8) {
			t.Fatalf("%dx%d: %v", tc.w, tc.h, m)
		}
		ratio := m["aspectRatio"].(float64)
		switch ratio {
		case 1.33, 1.66, 1.78, 1.85, 2.2, 2.35:
		default:
			t.Errorf("noncanonical aspect %v", ratio)
		}
	}
}

func TestPMSPixelAspectAndVideoIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		sar                 string
		present, anamorphic bool
	}{{"", false, false}, {"1:1", false, false}, {"333:320", true, false}, {"21:20", true, false}, {"16:15", true, true}, {"1:2", true, true}} {
		f := newFixture()
		f.playback.info.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
		video := &f.playback.info.Streams[0]
		video.SampleAspectRatio, video.CodedHeight, video.CodecTag, video.Profile, video.Level = tc.sar, 1080, "avc1", "0", -99
		r := f.get(t, "/library/metadata/1000000000000550", jsonAccept...)
		f.srv.Close()
		if r.Code != 200 {
			t.Fatal(r.Code)
		}
		st := decodeJSON(t, r)["Metadata"].([]any)[0].(map[string]any)["Media"].([]any)[0].(map[string]any)["Part"].([]any)[0].(map[string]any)["Stream"].([]any)[0].(map[string]any)
		_, present := st["pixelAspectRatio"]
		if present != tc.present {
			t.Errorf("SAR%s: %v", tc.sar, st)
		}
		if tc.present && (st["pixelAspectRatio"] != tc.sar || st["anamorphic"] != tc.anamorphic) {
			t.Errorf("SAR%s: %v", tc.sar, st)
		}
		if st["codedHeight"] != float64(1088) {
			t.Errorf("coded height: %v", st)
		}
		for _, name := range []string{"codecID", "profile", "level"} {
			if _, ok := st[name]; ok {
				t.Errorf("unexpected%s: %v", name, st[name])
			}
		}
	}
}

func TestPMSChapterProjectionAndPlayerRefusal(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.info.Chapters = []core.Chapter{{ID: -99, Title: "Opening", Start: 5 * time.Millisecond, End: 45 * time.Second}}
	r := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&hasMDE=1&directPlay=1", "X-Plex-Client-Profile-Name", "Chrome")
	if r.Code != 200 {
		t.Fatalf("decision %d %s", r.Code, r.Body.String())
	}
	for _, flag := range []string{"", "?includeChapters=0", "?includeChapters=1"} {
		r := f.get(t, "/library/metadata/1000000000000550"+flag, jsonAccept...)
		if r.Code != 200 {
			t.Fatal(r.Code)
		}
		md := decodeJSON(t, r)["Metadata"].([]any)[0].(map[string]any)
		chapters, present := md["Chapter"]
		if md["chapterSource"] != "media" || present != (flag == "?includeChapters=1") {
			t.Fatalf("%s projection: %v", flag, md)
		}
		if present {
			c := chapters.([]any)[0].(map[string]any)
			if c["id"].(float64) <= 0 || c["startTimeOffset"] != float64(5) {
				t.Fatalf("chapter %v", c)
			}
		}
	}
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		r := f.request(t, method, "/player/playback/playMedia", nil)
		if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "400") {
			t.Errorf("%s player refusal%d", method, r.Code)
		}
	}
}
