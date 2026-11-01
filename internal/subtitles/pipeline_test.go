package subtitles

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/braedonsaunders/ether/internal/core"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMovieHashEdges(t *testing.T) {
	raw := make([]byte, 200000)
	binary.LittleEndian.PutUint64(raw[0:8], 3)
	binary.LittleEndian.PutUint64(raw[len(raw)-8:], 9)
	hash, err := MovieHash(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || hash != "0000000000030d4c" {
		t.Fatalf("hash %s %v", hash, err)
	}
	if _, err = MovieHash(bytes.NewReader(raw[:100]), 200000); err == nil {
		t.Fatal("short ranges accepted")
	}
	if _, err = MovieHash(bytes.NewReader(raw), 100); err == nil {
		t.Fatal("small file accepted")
	}
}
func TestASSNormalizeAndAlign(t *testing.T) {
	raw := []byte("[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.50,0:00:03.00,Default,,0,0,0,,{\\i1}Hello, world\\NNext line\n")
	srt, vtt, err := Normalize(raw)
	if err != nil || !strings.Contains(srt, "Hello, world\nNext line") || !strings.Contains(vtt, "00:00:01.500") {
		t.Fatalf("normalize %q %q %v", srt, vtt, err)
	}
	cues := []Cue{{time.Second, 2 * time.Second, "This is the first phrase"}, {3 * time.Second, 4 * time.Second, "This is the second phrase"}, {6 * time.Second, 7 * time.Second, "This is the final phrase"}}
	ref, _ := Shift(cues, 2500*time.Millisecond, 1)
	fit, ok := FitTiming(cues, ref)
	if !ok || absDuration(fit.Offset-2500*time.Millisecond) > timingFrameSlack {
		t.Fatalf("offset %s ok=%v", fit.Offset, ok)
	}
	ref[0].Start += 5 * time.Second
	ref[1].Start += 8 * time.Second
	if _, ok = FitTiming(cues, ref); ok {
		t.Fatal("inconsistent references accepted")
	}
}
func TestHashSearchRank(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("moviehash") != "000000000000abcd" {
			t.Error("missing hash")
		}
		io.WriteString(w, `{"data":[{"attributes":{"language":"en","download_count":100000,"files":[{"file_id":1}]}},{"attributes":{"language":"en","moviehash_match":true,"files":[{"file_id":2}]}}]}`)
	}))
	defer srv.Close()
	s := New(Options{BaseURL: srv.URL})
	c, err := s.SearchWithHash(context.Background(), &core.Title{Key: core.MovieKey(550)}, "en", "", "000000000000abcd")
	if err != nil || len(c) != 2 || c[0].FileID != 2 {
		t.Fatalf("hash rank %+v %v", c, err)
	}
}
