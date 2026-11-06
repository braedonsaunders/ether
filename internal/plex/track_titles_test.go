package plex

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"strings"
	"testing"
)

func TestTrackTitleCompositionAndClientProjection(t *testing.T) {
	for _, tc := range []struct {
		st           core.StreamInfo
		locale, want string
	}{
		{core.StreamInfo{Type: "subtitle", Codec: "srt", Language: "en", Title: "English SDH", HearingImpaired: model.Ptr(true)}, "en", "English SDH (SRT)"},
		{core.StreamInfo{Type: "subtitle", Codec: "srt", Language: "en", Title: "Signs & Songs", Forced: true}, "en", "Signs & Songs (English SRT)"},
		{core.StreamInfo{Type: "subtitle", Codec: "srt", Language: "fr", Title: "French"}, "en", "French (French SRT)"},
		{core.StreamInfo{Type: "subtitle", Codec: "srt", Language: "en", External: true, ProviderTitle: "OpenSubtitles"}, "en", "English (SRT OpenSubtitles)"},
		{core.StreamInfo{Type: "subtitle", Codec: "eia_608", Language: "en"}, "en", "English (Closed Captions)"},
		{core.StreamInfo{Type: "audio", Codec: "aac", Channels: 2, Language: "en", Title: "English AAC 2.0"}, "en", "English AAC 2.0"},
	} {
		if got := composeTrackTitle(tc.st, tc.locale, true); got != tc.want {
			t.Errorf("%+v: %q want %q", tc.st, got, tc.want)
		}
	}
	f := newFixture()
	defer f.srv.Close()
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "video", Codec: "h264", Width: 1920, Height: 1080, SampleAspectRatio: "333:320", Dub: model.Ptr(true), HearingImpaired: model.Ptr(true)},
		{Type: "subtitle", Codec: "srt", Language: "fr", HearingImpaired: model.Ptr(true), VisualImpaired: model.Ptr(true)},
	}}
	for _, product := range []string{"Plex Web", "Plex HTPC for Linux", "Plex HTPC", "plex web", ""} {
		streams, src := f.srv.renderStreams(42, info, selection{}, nil)
		projectStreamTitles(streams, src, info, &Request{Product: product, Language: "de"})
		if streams[0].Default != nil || streams[0].HearingImpaired != nil || streams[0].Dub == nil {
			t.Fatalf("video dispositions: %+v", streams[0])
		}
		if streams[0].Anamorphic == nil || *streams[0].Anamorphic || streams[0].PixelAspectRatio != "333:320" {
			t.Fatalf("SAR %+v", streams[0])
		}
		want := "Französisch SDH"
		if codecInPlainTitle(product) {
			want += " (SRT)"
		}
		if streams[1].DisplayTitle != want || streams[1].VisualImpaired == nil {
			t.Errorf("%s: %+v want %s", product, streams[1], want)
		}
	}
	for _, code := range []string{"cha", "guj", "hye", "kat", "mar", "mdr", "mon", "mya", "nno", "pan", "sin", "yor", "zul"} {
		if languageTag(code) == code && code != "mdr" {
			t.Errorf("unmapped language %s", code)
		}
	}
	if languageCode3("fre") != "fra" || languageCode3("english") != "" || languageTag("und") != "" {
		t.Fatal("language normalization")
	}
}

func TestTrackPresentationHTTP(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = core.MediaInfo{Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Index: 1, Type: "subtitle", Codec: "srt", Language: "fr", Forced: true, HearingImpaired: model.Ptr(true), VisualImpaired: model.Ptr(true)},
	}}
	for _, product := range []string{"Plex Web", "Plex HTPC"} {
		rec := f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Product", product, "X-Plex-Language", "de")
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		expected := `displayTitle="Französisch Erzwungen SDH"`
		if product == "Plex Web" {
			expected = `displayTitle="Französisch Erzwungen SDH (SRT)"`
		}
		if !strings.Contains(body, expected) || !strings.Contains(body, `visualImpaired="1"`) {
			t.Fatal(body)
		}
	}
}
