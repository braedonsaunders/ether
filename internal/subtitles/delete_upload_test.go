package subtitles

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestDeleteUploadFencesTitleAndReplacement(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	title := &core.Title{Key: core.MovieKey(550)}
	store := func(text string) core.StreamInfo {
		st, err := s.StoreUpload(title, "movie.en.srt", "srt", "en", []byte("1\n00:00:01,000 --> 00:00:02,000\n"+text+"\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	old := store("old")
	current := store("current")
	if err := s.DeleteUpload(title.Key, old); err == nil {
		t.Fatal("stale stream removed replacement")
	}
	if err := s.DeleteUpload(core.MovieKey(1), current); err == nil {
		t.Fatal("foreign title removed upload")
	}
	if err := s.DeleteUpload(title.Key, current); err != nil {
		t.Fatal(err)
	}
	if rows := s.ManualSidecars(title, nil); len(rows) != 0 {
		t.Fatalf("deleted upload still listed: %+v", rows)
	}
	if r, _, err := s.Open(current.ExternalURL); err == nil {
		r.Close()
		t.Fatal("deleted bytes still accessible")
	}
	if err := s.DeleteUpload(title.Key, current); err == nil {
		t.Fatal("repeated delete accepted")
	}
}
