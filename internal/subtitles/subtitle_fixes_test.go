package subtitles

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

// pt-BR is a distinct language from European Portuguese: an embedded pt-PT
// track must not satisfy a pt-BR preference, while a pob/pt-BR track must.
func TestSubtitleLanguagePtBRMissing(t *testing.T) {
	user := &core.User{SubtitleLanguages: []string{"pt-BR"}}
	miss := MissingLanguages(user, []core.StreamInfo{{Type: "subtitle", Language: "pt-PT"}})
	if len(miss) != 1 || miss[0] != "pt-br" {
		t.Fatalf("pt-PT must not satisfy pt-BR: missing=%v", miss)
	}
	for _, have := range []string{"pt-br", "pt_BR", "pob"} {
		if miss := MissingLanguages(user, []core.StreamInfo{{Type: "subtitle", Language: have}}); len(miss) != 0 {
			t.Fatalf("embedded %q must satisfy pt-BR: missing=%v", have, miss)
		}
	}
	// zh-TW is likewise distinct from generic Chinese.
	miss = MissingLanguages(&core.User{SubtitleLanguages: []string{"zh-TW"}}, []core.StreamInfo{{Type: "subtitle", Language: "zh-CN"}})
	if len(miss) != 1 || miss[0] != "zh-tw" {
		t.Fatalf("zh-CN must not satisfy zh-TW: missing=%v", miss)
	}
}

// A hearing-impaired sidecar must carry the SDH marker in its generated
// title, so trackprefs' sdh() detection (which reads Title) can match it;
// non-SDH titles keep their existing format.
func TestSidecarSDHTitleAndSelect(t *testing.T) {
	ctx := context.Background()
	f := newOSFake(t)
	f.hearing = true
	dir := t.TempDir()
	svc := newSvc(t, f, config.OpenSubtitles{}, dir)
	title := &core.Title{Key: core.MovieKey(42), Type: core.TypeMovie}
	user := &core.User{SubtitleLanguages: []string{"en"}}
	out, err := svc.Sidecars(ctx, title, user, nil)
	if err != nil || len(out) != 1 {
		t.Fatalf("sidecars: %+v %v", out, err)
	}
	if !strings.HasSuffix(out[0].Title, " (SDH)") {
		t.Fatalf("SDH sidecar title lacks delimited SDH marker: %q", out[0].Title)
	}
	if got, want := sidecarTitle("en", "Some.Release"), "EN (OpenSubtitles: Some.Release)"; got != want {
		t.Fatalf("non-SDH title changed: got %q want %q", got, want)
	}

	// The remembered selection (fresh service over the same dir) keeps SDH.
	svc2 := newSvc(t, f, config.OpenSubtitles{}, dir)
	out2, err := svc2.Sidecars(ctx, title, user, nil)
	if err != nil || len(out2) != 1 || !strings.HasSuffix(out2[0].Title, " (SDH)") {
		t.Fatalf("remembered SDH sidecar: %+v %v", out2, err)
	}

	// trackprefs with style=sdh must pick the real generated sidecar title
	// over a plain track of the same language listed first.
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prefs := trackprefs.New(db)
	r := trackprefs.Default()
	r.SubtitleLanguages = []string{"en"}
	r.SubtitleMode = "always"
	r.SubtitleStyle = "sdh"
	if err := prefs.PutRules(ctx, 0, r); err != nil {
		t.Fatal(err)
	}
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "subtitle", Language: "en", Title: "English"},
		out[0],
	}}
	sel, err := prefs.Select(ctx, 7, title.Key, info, "")
	if err != nil || sel.Subtitle != 1 {
		t.Fatalf("sdh style must select the SDH sidecar: %+v %v (title %q)", sel, err, out[0].Title)
	}
}

// en-GB/en_US fold to en in this package (only pt/zh keep a region).
func TestSubtitleLanguageRegionFoldingEn(t *testing.T) {
	for _, c := range []string{"en-GB", "en_US", "EN-gb"} {
		if got := normLang(c); got != "en" {
			t.Fatalf("normLang(%q)=%q want en", c, got)
		}
	}
	user := &core.User{SubtitleLanguages: []string{"en-GB"}}
	if miss := MissingLanguages(user, []core.StreamInfo{{Type: "subtitle", Language: "en_US"}}); len(miss) != 0 {
		t.Fatalf("en_US embedded must satisfy en-GB preference: missing=%v", miss)
	}
}

// Mutation target: aligner.go saveSelection must carry SDH: c.HearingImp.
// Without it, a selection saved during alignment reloads as non-SDH and its
// sidecar loses the " (SDH)" marker.
func TestAlignerSelectionPersistsSDH(t *testing.T) {
	ctx := context.Background()
	f := newOSFake(t)
	f.hearing = true
	dir := t.TempDir()
	svc := newSvc(t, f, config.OpenSubtitles{}, dir)
	title := &core.Title{Key: core.MovieKey(42), Type: core.TypeMovie}
	a := svc.TimelineAligner()
	bs := &buildState{searches: map[string][]Candidate{}}
	const release = "Show.S01E02.1080p.WEB"
	if tls := a.timelines(ctx, bs, title, "en", release, 0); len(tls) == 0 {
		t.Fatal("aligner built no timeline from the SDH candidate")
	}
	sel, ok := svc.loadSelection(title, "en", release)
	if !ok {
		t.Fatal("aligner did not persist a selection")
	}
	if !sel.SDH {
		t.Fatalf("aligner-persisted selection lost SDH: %+v", sel)
	}
	if st := sidecarStream(0, "en", sel); !strings.HasSuffix(st.Title, " (SDH)") {
		t.Fatalf("sidecar from aligner selection lacks SDH marker: %q", st.Title)
	}
}

// Mutation target: ondemand.go DownloadOnDemand must carry
// SDH: o.cand.HearingImp into the persisted manual selection.
func TestDownloadOnDemandSDH(t *testing.T) {
	ctx := context.Background()
	f := newOSFake(t)
	f.hearing = true
	svc := newSvc(t, f, config.OpenSubtitles{}, t.TempDir())
	if _, err := svc.SearchOnDemand(ctx, episode, "en", ""); err != nil {
		t.Fatal(err)
	}
	st, err := svc.DownloadOnDemand(ctx, episode, 11)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(st.Title, " (SDH)") {
		t.Fatalf("DownloadOnDemand SDH stream lacks delimited marker: %q", st.Title)
	}
	sel, ok := svc.manualSelections(episode.Key)["en"]
	if !ok || !sel.SDH {
		t.Fatalf("persisted manual selection lost SDH: %+v ok=%v", sel, ok)
	}
	if got := svc.CachedSidecars(episode, &core.User{SubtitleLanguages: []string{"en"}}, nil); len(got) != 1 || !strings.HasSuffix(got[0].Title, " (SDH)") {
		t.Fatalf("cached SDH pick: %+v", got)
	}
}

// Compat (pré-fix): a persisted selection written before the sdh field
// existed loads as non-SDH — no SDH is invented from the release name —
// and serves until a re-download rewrites the selection with sdh set.
func TestOldSelectionWithoutSDHLoadsAsNonSDH(t *testing.T) {
	f := newOSFake(t)
	dir := t.TempDir()
	svc := newSvc(t, f, config.OpenSubtitles{}, dir)
	title := &core.Title{Key: core.MovieKey(77), Type: core.TypeMovie}
	const id = "oldsel1"
	for _, ext := range []string{"srt", "vtt"} {
		if err := os.WriteFile(filepath.Join(dir, id+"."+ext), []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := `{"id":"oldsel1","fileId":11,"language":"en","release":"Old.Rel","at":"2024-01-01T00:00:00Z"}`
	p := svc.indexPath(title, "en", "Old.Rel")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	sel, ok := svc.loadSelection(title, "en", "Old.Rel")
	if !ok || sel.SDH {
		t.Fatalf("old selection must load as non-SDH: %+v ok=%v", sel, ok)
	}
	if st := sidecarStream(0, "en", sel); strings.HasSuffix(st.Title, " (SDH)") {
		t.Fatalf("old selection must not gain an invented SDH marker: %q", st.Title)
	}
	// Re-download rewrites the selection, now with the reliable signal.
	svc.saveSelection(title, "en", "Old.Rel", selection{ID: id, FileID: 11, Language: "en", Release: "Old.Rel", SDH: true})
	if sel, ok := svc.loadSelection(title, "en", "Old.Rel"); !ok || !sel.SDH {
		t.Fatalf("rewritten selection must carry SDH: %+v ok=%v", sel, ok)
	}
}

func makeSubtitleZip(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const zipSRT = "1\n00:00:01,000 --> 00:00:02,000\n%s\n\n"

// ZIP: macOS junk is skipped even when it contains valid cues, .srt is
// preferred over .ass, an entry without cues falls through to the next,
// and an oversized entry is an explicit error, not a silent truncation.
func TestUnzipSubtitleJunkPreferenceFallbackSize(t *testing.T) {
	srt := func(text string) string { return strings.Replace(zipSRT, "%s", text, 1) }
	ass := "[Script Info]\nTitle: f\nScriptType: v4.00+\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,ASSGOOD\n"

	raw := makeSubtitleZip(t,
		[2]string{"__MACOSX/movie.srt", srt("MACJUNK")},
		[2]string{"._movie.srt", srt("APPLEDOUBLE")},
		[2]string{".DS_Store", "junk"},
		[2]string{"subs/", ""},
		[2]string{"movie.srt", srt("GOOD")},
	)
	got, err := unzipSubtitle(raw, "en")
	if err != nil || !strings.Contains(string(got), "GOOD") {
		t.Fatalf("macOS junk must be skipped: %q %v", got, err)
	}

	raw = makeSubtitleZip(t, [2]string{"a.ass", ass}, [2]string{"b.srt", srt("SRTGOOD")})
	got, err = unzipSubtitle(raw, "en")
	if err != nil || !strings.Contains(string(got), "SRTGOOD") {
		t.Fatalf(".srt must be preferred over .ass: %q %v", got, err)
	}

	raw = makeSubtitleZip(t, [2]string{"a.srt", "not a subtitle at all"}, [2]string{"b.srt", srt("FALLBACK")})
	got, err = unzipSubtitle(raw, "en")
	if err != nil || !strings.Contains(string(got), "FALLBACK") {
		t.Fatalf("cue-less entry must fall through to the next: %q %v", got, err)
	}

	big := makeSubtitleZip(t, [2]string{"big.srt", strings.Repeat("x", (16<<20)+1)})
	if _, err := unzipSubtitle(big, "en"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf(">16MiB entry must be an explicit too-large error, got %v", err)
	}
}

// Zips holding .ass/.ssa must be accepted: unzipSubtitle returns the raw
// ASS bytes and NormalizeLang converts them.
func TestUnzipSubtitleAssSSA(t *testing.T) {
	ass := "[Script Info]\nTitle: fixture\nScriptType: v4.00+\n\n[Events]\n" +
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,Olá mundo\n"
	for _, ext := range []string{".ass", ".ssa"} {
		var zbuf bytes.Buffer
		zw := zip.NewWriter(&zbuf)
		w, err := zw.Create("Movie.pt-br" + ext)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(ass)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		raw, err := unzipSubtitle(zbuf.Bytes(), "pt-BR")
		if err != nil {
			t.Fatalf("%s: unzipSubtitle: %v", ext, err)
		}
		srt, vtt, err := NormalizeLang(raw, "pt-BR")
		if err != nil {
			t.Fatalf("%s: NormalizeLang: %v", ext, err)
		}
		if !strings.Contains(srt, "Olá mundo") || !strings.Contains(srt, "00:00:01,000 --> 00:00:03,000") {
			t.Fatalf("%s: converted srt: %q", ext, srt)
		}
		if !strings.Contains(vtt, "Olá mundo") {
			t.Fatalf("%s: converted vtt: %q", ext, vtt)
		}
	}
}
