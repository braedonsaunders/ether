package subtitles

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestStoreUploadListsAsManualSidecar(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	title := &core.Title{Key: core.MovieKey(550)}
	body := []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n\n")
	st, err := s.StoreUpload(title, "movie.fr.srt", "srt", "", body)
	if err != nil {
		t.Fatal(err)
	}
	if st.Language != "fr" || !st.External || !strings.HasPrefix(st.ExternalURL, URLPrefix) {
		t.Fatalf("stream %+v", st)
	}
	listed := s.ManualSidecars(title, nil)
	if len(listed) != 1 || listed[0].ExternalURL != st.ExternalURL {
		t.Fatalf("listed %+v", listed)
	}
	r, format, err := s.Open(st.ExternalURL)
	if err != nil || format != "srt" {
		t.Fatalf("open %s %v", format, err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !strings.Contains(string(got), "Hello") {
		t.Fatalf("body %q", got)
	}
}

func TestStoreUploadDoesNotReplaceManualPick(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	title := &core.Title{Key: core.MovieKey(550)}
	body := []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n\n")
	manual := selection{ID: "picked", FileID: 9, Language: "fr", Release: "Picked", At: s.now().UTC()}
	p := s.manualPath(title.Key, "fr")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(manual)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "picked.srt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "picked.vtt"), []byte("WEBVTT\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StoreUpload(title, "movie.fr.srt", "srt", "fr", body); err != nil {
		t.Fatal(err)
	}
	got := s.manualSelections(title.Key)
	if got["fr"].ID != "picked" {
		t.Fatalf("manual pick overwritten: %+v", got)
	}
}

func TestStoreUploadRejectsBadFormat(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	title := &core.Title{Key: core.MovieKey(11)}
	if _, err := s.StoreUpload(title, "x.pgs", "pgs", "en", []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n\n")); err == nil {
		t.Fatal("pgs accepted")
	}
}

func TestAcceptsUploadFormat(t *testing.T) {
	for _, format := range []string{"srt", "vtt", "ass", "ssa", "smi", "txt", "psb"} {
		if !AcceptsUploadFormat(format) {
			t.Fatalf("%s rejected", format)
		}
	}
	for _, format := range []string{"SRT", "sub", "idx", "sup", "pgs", ""} {
		if AcceptsUploadFormat(format) {
			t.Fatalf("%s accepted", format)
		}
	}
}
