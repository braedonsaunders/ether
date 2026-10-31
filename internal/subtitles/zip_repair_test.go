package subtitles

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestZipCandidateSelectionRegressions(t *testing.T) {
	raw := makeZipSel(t,
		[2]string{"movie.en.forced.srt", srtCuesSel("FORCED")},
		[2]string{"movie.en.sdh.srt", srtCuesSel("SDH")},
		[2]string{"movie.en.normal.srt", srtCuesSel("NORMAL1", "NORMAL2")},
	)
	for _, tc := range []struct {
		name string
		c    Candidate
		want string
	}{
		{"forced", Candidate{Language: "en", Forced: true}, "FORCED"},
		{"sdh", Candidate{Language: "en", HearingImp: true}, "SDH"},
		{"normal", Candidate{Language: "en"}, "NORMAL2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unzipSubtitleCandidate(raw, tc.c)
			if err != nil || !strings.Contains(string(got), tc.want) {
				t.Fatalf("got %q: %v", got, err)
			}
		})
	}
	// An explicit foreign-language suffix cannot beat an unlabelled file.
	raw = makeZipSel(t, [2]string{"movie.fr.srt", srtCuesSel("FRENCH1", "FRENCH2")}, [2]string{"movie.srt", srtCuesSel("UNKNOWN")})
	got, err := unzipSubtitleCandidate(raw, Candidate{Language: "en"})
	if err != nil || !strings.Contains(string(got), "UNKNOWN") {
		t.Fatalf("got %q: %v", got, err)
	}
	// Labels in either order must still reject conflicting metadata.
	raw = makeZipSel(t, [2]string{"movie.forced.en.srt", srtCuesSel("FORCED")})
	if _, err = unzipSubtitleCandidate(raw, Candidate{Language: "en"}); err == nil {
		t.Fatal("reversed forced label accepted as normal")
	}
	raw = makeZipSel(t, [2]string{"The.It.srt", srtCuesSel("TITLE")})
	if _, err = unzipSubtitleCandidate(raw, Candidate{Language: "en"}); err != nil {
		t.Fatal("title word misidentified as Italian:", err)
	}
	// Common title words are not language labels, and explicit conflicts cannot be relabelled.
	raw = makeZipSel(t, [2]string{"fr.the.movie.srt", srtCuesSel("TITLE")})
	if _, err = unzipSubtitleCandidate(raw, Candidate{Language: "en"}); err != nil {
		t.Fatal(err)
	}
	raw = makeZipSel(t, [2]string{"movie.en.forced.srt", srtCuesSel("FORCED")})
	if _, err = unzipSubtitleCandidate(raw, Candidate{Language: "en"}); err == nil {
		t.Fatal("conflicting forced entry accepted as normal")
	}
}

func TestZipCorruptExpansionBudget(t *testing.T) {
	var entries [][2]string
	for i := 0; i < 3; i++ {
		entries = append(entries, [2]string{fmt.Sprintf("%c.srt", 'a'+i), strings.Repeat("x", 12<<20)})
	}
	entries = append(entries, [2]string{"z.srt", srtCuesSel("AFTER_BUDGET")})
	raw := makeZipSel(t, entries...)
	pos := 0
	for i := 0; i < 3; i++ {
		n := bytes.Index(raw[pos:], []byte{'P', 'K', 1, 2})
		if n < 0 {
			t.Fatal("missing directory")
		}
		pos += n
		binary.LittleEndian.PutUint32(raw[pos+16:], 0)
		pos += 4
	}
	if _, err := unzipSubtitleCandidate(raw, Candidate{Language: "en"}); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected budget error, got %v", err)
	}
}

func TestZipSelectionCallsites(t *testing.T) {
	for _, tc := range []struct {
		name        string
		forced, sdh bool
		label       string
		pref        Preferences
	}{
		{"forced", true, false, "FORCED", Preferences{Mode: "forced-only"}},
		{"sdh", false, true, "SDH", Preferences{Mode: "always", Style: "sdh"}},
		{"normal", false, false, "NORMAL", Preferences{Mode: "always", Style: "normal"}},
	} {
		for _, manual := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/manual=%v", tc.name, manual), func(t *testing.T) {
				raw := makeZipSel(t, [2]string{"movie.en.forced.srt", srtCuesSel("FORCED")}, [2]string{"movie.en.sdh.srt", srtCuesSel("SDH")}, [2]string{"movie.en.normal.srt", srtCuesSel("NORMAL1", "NORMAL2")})
				var server *httptest.Server
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/subtitles":
						json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"attributes": map[string]any{"language": "en", "foreign_parts_only": tc.forced, "hearing_impaired": tc.sdh, "files": []any{map[string]any{"file_id": 1, "file_name": "movie.zip"}}}}}})
					case "/download":
						json.NewEncoder(w).Encode(map[string]any{"link": server.URL + "/file"})
					case "/file":
						w.Write(raw)
					default:
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				svc := New(Options{Config: config.OpenSubtitles{Enabled: true, APIKey: "k"}, CacheDir: t.TempDir(), BaseURL: server.URL})
				title := forcedTitle(852)
				var st core.StreamInfo
				if manual {
					_, err := svc.SearchOnDemand(context.Background(), title, "en", "")
					if err != nil {
						t.Fatal(err)
					}
					st, err = svc.DownloadOnDemand(context.Background(), title, 1)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					out, err := svc.SidecarsForRelease(context.Background(), title, &core.User{SubtitleLanguages: []string{"en"}}, nil, "", tc.pref)
					if err != nil || len(out) != 0 {
						t.Fatalf("preference must not download: %+v %v", out, err)
					}
					return
				}
				if st.Forced != tc.forced || strings.Contains(st.Title, "SDH") != tc.sdh || !strings.Contains(openText(t, svc, st), tc.label) {
					t.Fatalf("mismatched stream: %+v text %q", st, openText(t, svc, st))
				}
				cached := svc.CachedSidecars(title, nil, nil)
				if manual && (len(cached) != 1 || cached[0].Forced != tc.forced || strings.Contains(cached[0].Title, "SDH") != tc.sdh) {
					t.Fatalf("persisted: %+v", cached)
				}
			})
		}
	}
}
