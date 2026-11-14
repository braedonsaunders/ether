package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/subtext"
)

type renditionFixture struct{ core.HLSSession }

func (renditionFixture) HasTextSubtitle(index int) bool { return index == 2 || index == 3 }
func (renditionFixture) TextSubtitleCues(int, time.Duration, time.Duration) ([]subtext.Cue, error) {
	return nil, nil
}
func (renditionFixture) ServeTextSubtitlePlaylist(http.ResponseWriter, *http.Request, int)     {}
func (renditionFixture) ServeTextSubtitleSegment(http.ResponseWriter, *http.Request, int, int) {}

// The master must advertise only playback-owned text outputs, retain selection
// and forced flags, escape authored names, and scope every child to its grant.
func TestPlexTextSubtitleRenditions(t *testing.T) {
	s := &Server{}
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Index: 2, Type: "subtitle", Codec: "srt", Language: "eng", Title: "a\"\nINJECT"},
		{Index: 3, Type: "subtitle", Codec: "srt", Language: "eng", Forced: true},
		{Index: 4, Type: "subtitle", Codec: "pgs"},
		{Index: 5, Type: "subtitle", Codec: "srt", External: true},
	}}
	entry := &transcodeEntry{subtitle: 1, streamToken: "playback-grant"}
	req := httptest.NewRequest(http.MethodGet, "/master.m3u8", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, &Request{Platform: "tvOS", Product: "Plex for Apple TV"}))
	tags, group := s.textRenditionTags(req, "play", renditionFixture{}, entry, info, core.Decision{Container: "mpegts"})
	if group != ",SUBTITLES=\"text\"" || strings.Count(tags, "#EXT-X-MEDIA:") != 2 || strings.Contains(tags, "\nINJECT") || !strings.Contains(tags, "FORCED=YES") || !strings.Contains(tags, "DEFAULT=YES") || !strings.Contains(tags, "playback-grant") || !strings.Contains(tags, "text-2/index.m3u8") {
		t.Fatalf("invalid rendition tags: %q %q", tags, group)
	}
	if tags, group = s.textRenditionTags(req, "play", renditionFixture{}, entry, info, core.Decision{Container: "mkv"}); tags != "" || group != "" {
		t.Fatal("Matroska must retain native subtitle muxing")
	}
	if tags, group = s.textRenditionTags(req, "play", renditionFixture{}, entry, info, core.Decision{BurnSubtitles: true}); tags != "" || group != "" {
		t.Fatal("burned subtitle advertised twice")
	}
	f := newFixture()
	defer f.srv.Close()
	for _, format := range []string{"ass", "srt", "vtt"} {
		t.Run(format, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f.srv.serveCueSubtitle(rec, httptest.NewRequest(http.MethodGet, "/subtitle", nil), &finiteCueFixture{live: true}, 2, "play", 0, format, false)
			body := rec.Body.String()
			if !strings.Contains(body, "first cue") || !strings.Contains(body, "second cue") {
				t.Fatal(body)
			}
			if format == "vtt" && strings.Count(body, "WEBVTT") != 1 {
				t.Fatal(body)
			}
			if format == "srt" && (!strings.Contains(body, "1\n00:00:01,000") || !strings.Contains(body, "2\n00:00:03,000")) {
				t.Fatal(body)
			}
			if format == "ass" && strings.Count(body, "[Events]") != 1 {
				t.Fatal(body)
			}
		})
	}
}

// A finite fake playback output lets the format checks observe a complete
// stream while verifying that the first batch is rendered without extraction.
type finiteCueFixture struct {
	renditionFixture
	live bool
}

func (f *finiteCueFixture) HasTextSubtitle(int) bool { return f.live }
func (f *finiteCueFixture) TextSubtitleCues(int, time.Duration, time.Duration) ([]subtext.Cue, error) {
	f.live = false
	return []subtext.Cue{{Start: time.Second, End: 2 * time.Second, Text: "first cue"}, {Start: 3 * time.Second, End: 4 * time.Second, Text: "second cue"}}, nil
}
