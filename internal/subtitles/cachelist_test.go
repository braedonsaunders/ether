package subtitles

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func writeCacheEntry(t *testing.T, dir, id, meta string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".srt"), []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The cache meta has no title: Key must come from the selection index,
// FileName from the sidecar on disk, and FetchedAt from the meta.
func TestCachedSubtitlesKeyFileNameAndFetchedAt(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	fetched := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	writeCacheEntry(t, s.dir, "abc123",
		`{"fileId":7,"language":"pt-br","release":"Some.Movie.2024.1080p.WEB-DL","fetchedAt":"`+fetched.Format(time.RFC3339)+`"}`)
	writeCacheEntry(t, s.dir, "orphan99",
		`{"fileId":9,"language":"en","release":"Other.Movie.2023.720p","fetchedAt":"`+fetched.Add(-time.Hour).Format(time.RFC3339)+`"}`)

	// Index entry for the episode this subtitle was remembered for.
	title := &core.Title{Key: core.EpisodeKey(1399, 1, 2)}
	s.saveSelection(title, "pt-br", "Some.Movie.2024.1080p.WEB-DL",
		selection{ID: "abc123", FileID: 7, Language: "pt-br", Release: "Some.Movie.2024.1080p.WEB-DL", At: fetched})

	items, err := s.CachedSubtitles()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	byID := map[string]CachedSubtitle{}
	for _, it := range items {
		byID[it.ID] = it
	}
	got := byID["abc123"]
	if got.Key != title.Key.String() {
		t.Fatalf("key = %q, want ratingKey %q from index", got.Key, title.Key.String())
	}
	if got.FileName != "abc123.srt" {
		t.Fatalf("fileName = %q", got.FileName)
	}
	if !got.FetchedAt.Equal(fetched) || got.Language != "pt-br" || got.Release != "Some.Movie.2024.1080p.WEB-DL" {
		t.Fatalf("meta fields lost: %+v", got)
	}
	// No index entry -> no key is invented.
	if orphan := byID["orphan99"]; orphan.Key != "" || orphan.FileName != "orphan99.srt" {
		t.Fatalf("orphan = %+v", orphan)
	}
	// Newest first.
	if items[0].ID != "abc123" {
		t.Fatalf("order = %+v", items)
	}
}

// Without fetchedAt in the meta, FetchedAt falls back to the .srt mtime.
func TestCachedSubtitlesFetchedAtFallsBackToMtime(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	writeCacheEntry(t, s.dir, "nomtime1", `{"fileId":1,"language":"en","release":"X.2024"}`)
	mtime := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(s.dir, "nomtime1.srt"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	items, err := s.CachedSubtitles()
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v err = %v", items, err)
	}
	if !items[0].FetchedAt.Equal(mtime) {
		t.Fatalf("fetchedAt = %v, want mtime %v", items[0].FetchedAt, mtime)
	}
}
