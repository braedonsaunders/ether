package mediabrowser

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestEmbyItemDetailUsesKnownSourceFacts(t *testing.T) {
	i := &instance{}
	m := map[string]any{}
	i.embyItemDetailDTO(m, frontend.ItemDetail{})
	if _, ok := m["Size"]; ok || m["SupportsSync"] != false {
		t.Fatalf("unknown source facts: %#v", m)
	}
	added := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	parts := 1
	i.embyItemDetailDTO(m, frontend.ItemDetail{Versions: []frontend.Version{{AddedAt: added, FileName: "movie.mp4", PartCount: &parts, Media: core.MediaInfo{
		Container: "mp4", Size: 123, BitrateKbps: 456, Streams: []core.StreamInfo{{Type: "video", Width: 1920, Height: 1080}}, Chapters: []core.Chapter{{Title: "Opening", Start: time.Second}},
	}}}})
	if _, ok := m["DateCreated"]; ok {
		t.Fatal("catalogue fallback was presented as a source creation date")
	}
	if m["FileName"] != "movie.mp4" || m["PartCount"] != 1 || m["Container"] != "mp4" || m["Size"] != int64(123) || m["Bitrate"] != 456000 || m["Width"] != 1920 || m["Height"] != 1080 {
		t.Fatalf("source facts: %#v", m)
	}
	chapter := m["Chapters"].([]any)[0].(map[string]any)
	if chapter["Name"] != "Opening" || chapter["StartPositionTicks"] != int64(10000000) || chapter["ChapterIndex"] != 0 {
		t.Fatalf("chapter facts: %#v", chapter)
	}
	ambiguous := map[string]any{}
	i.embyItemDetailDTO(ambiguous, frontend.ItemDetail{Versions: []frontend.Version{{Media: core.MediaInfo{Size: 123}}, {Media: core.MediaInfo{Size: 456}}}})
	if _, ok := ambiguous["Size"]; ok {
		t.Fatalf("selected an unspecified version: %#v", ambiguous)
	}
	unsafe := map[string]any{}
	i.embyItemDetailDTO(unsafe, frontend.ItemDetail{Versions: []frontend.Version{{FileName: "private/secret.mkv"}}})
	if _, ok := unsafe["FileName"]; ok {
		t.Fatal("source path leaked as a filename")
	}
}

func TestEmbyItemBaseDoesNotChangeJellyfin(t *testing.T) {
	title := frontend.Title{Title: "Example", SortTitle: "Example, The", Type: "movie", IDs: core.IDs{IMDB: "tt1234567", TMDB: 42}}
	emby := (&instance{protocol: "emby"}).itemDTO(title, frontend.WatchStatus{})
	jellyfin := (&instance{protocol: "jellyfin"}).itemDTO(title, frontend.WatchStatus{})
	if emby["LockData"] != false || emby["ForcedSortName"] != title.SortTitle {
		t.Fatalf("Emby base fields: %#v", emby)
	}
	if _, ok := jellyfin["LockData"]; ok {
		t.Fatalf("Emby field leaked to Jellyfin: %#v", jellyfin)
	}
	urls := emby["ExternalUrls"].([]any)
	if len(urls) != 2 || urls[0].(map[string]string)["Url"] != "https://www.imdb.com/title/tt1234567/" || urls[1].(map[string]string)["Url"] != "https://www.themoviedb.org/movie/42" {
		t.Fatalf("external URLs: %#v", urls)
	}
	invalid := (&instance{protocol: "emby"}).itemDTO(frontend.Title{IDs: core.IDs{IMDB: "tt1/../../unsafe"}}, frontend.WatchStatus{})
	if _, ok := invalid["ExternalUrls"]; ok {
		t.Fatal("invalid provider ID produced a URL")
	}
}
