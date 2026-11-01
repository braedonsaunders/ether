package subtitles

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func synthCues(n int, step, offset time.Duration, rate float64) []Cue {
	out := make([]Cue, n)
	for i := 0; i < n; i++ {
		start := time.Duration(float64(time.Duration(i)*step)*rate) + offset
		out[i] = Cue{Start: start, End: start + 800*time.Millisecond, Text: "x"}
	}
	return out
}

func irregularCues(n int, step, offset time.Duration) []Cue {
	out := make([]Cue, n)
	t := time.Duration(0)
	for i := 0; i < n; i++ {
		gap := time.Duration(float64(step) * (0.4 + float64((i*7)%19)/10))
		start := t + offset
		out[i] = Cue{Start: start, End: start + 700*time.Millisecond, Text: "x"}
		t += gap
	}
	return out
}

func shiftCues(cues []Cue, d time.Duration) []Cue {
	out := make([]Cue, len(cues))
	for i, c := range cues {
		out[i] = Cue{Start: c.Start + d, End: c.End + d, Text: c.Text}
	}
	return out
}

func TestFitTimingShift(t *testing.T) {
	ref := synthCues(16, 2*time.Second, 0, 1)
	cand := synthCues(16, 2*time.Second, 1500*time.Millisecond, 1)
	fit, ok := FitTiming(cand, ref)
	if !ok {
		t.Fatal("no fit")
	}
	if absDuration(fit.Offset+1500*time.Millisecond) > 40*time.Millisecond {
		t.Fatalf("offset %s", fit.Offset)
	}
	if absDuration(time.Duration((fit.Rate-1)*float64(time.Second))) > time.Millisecond {
		t.Fatalf("rate %v", fit.Rate)
	}
}

func TestFitTimingFrameRate(t *testing.T) {
	ref := synthCues(20, 2500*time.Millisecond, 0, 1)
	cand := synthCues(20, 2500*time.Millisecond, 0, fps25/fps23976)
	fit, ok := FitTiming(cand, ref)
	if !ok {
		t.Fatal("no fit")
	}
	want := fps23976 / fps25
	if absDuration(time.Duration((fit.Rate-want)*float64(time.Second))) > 2*time.Millisecond {
		t.Fatalf("rate %v want %v offset %s", fit.Rate, want, fit.Offset)
	}
	shifted, err := Shift(cand, fit.Offset, fit.Rate)
	if err != nil {
		t.Fatal(err)
	}
	if absDuration(shifted[5].Start-ref[5].Start) > timingFrameSlack {
		t.Fatalf("cue 5 %s vs %s", shifted[5].Start, ref[5].Start)
	}
}

func TestFitTimingRejectsUnrelated(t *testing.T) {
	ref := synthCues(16, 2*time.Second, 0, 1)
	cand := synthCues(16, 1800*time.Millisecond, 7*time.Second, 1.37)
	if _, ok := FitTiming(cand, ref); ok {
		t.Fatal("unrelated pattern accepted")
	}
}

func TestCorrectAppliesCachedFit(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	id := "movie-en"
	raw := []byte(RenderSRT(synthCues(8, time.Second, 1500*time.Millisecond, 1)))
	if err := os.WriteFile(filepath.Join(s.dir, id+".srt"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s.storeCorrection(id, "rel-a", correction{
		Offset: -1500 * time.Millisecond, Rate: 1, Applied: true, Measured: true, At: s.now().UTC(),
	})
	r, _, err := s.OpenFormatFrom(id, "srt", "rel-a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	cues, err := ParseCues(string(got))
	if err != nil || len(cues) == 0 {
		t.Fatalf("parse %v %q", err, got)
	}
	if absDuration(cues[0].Start) > timingFrameSlack {
		t.Fatalf("first cue %s", cues[0].Start)
	}
	plain, _, err := s.OpenFormatFrom(id, "srt", "rel-b")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := io.ReadAll(plain)
	plain.Close()
	uncorrected, _ := ParseCues(string(other))
	if absDuration(uncorrected[0].Start-1500*time.Millisecond) > timingFrameSlack {
		t.Fatalf("other release used this fit: %s", uncorrected[0].Start)
	}
}

func TestFitTimingLongFilmWithJitter(t *testing.T) {
	ref := irregularCues(1200, 2*time.Second, 0)
	cand := shiftCues(ref, 1800*time.Millisecond)
	for i := range cand {
		if i%17 == 0 {
			cand[i].Start += 80 * time.Millisecond
		}
	}
	fit, ok := FitTiming(cand, ref)
	if !ok {
		t.Fatal("no fit")
	}
	if absDuration(fit.Offset+1800*time.Millisecond) > 60*time.Millisecond {
		t.Fatalf("offset %s", fit.Offset)
	}
}

func TestFitTimingInSyncUnchanged(t *testing.T) {
	ref := irregularCues(40, 2*time.Second, 0)
	fit, ok := FitTiming(ref, ref)
	if !ok || !fit.Identity() {
		t.Fatalf("in-sync %+v ok=%v", fit, ok)
	}
}

func TestFitTimingRejectsDifferentCut(t *testing.T) {
	theatrical := irregularCues(80, 2*time.Second, 0)
	inserted := irregularCues(20, 2*time.Second, theatrical[40].Start)
	shift := 90 * time.Second
	extended := append(append([]Cue{}, theatrical[:40]...), inserted...)
	for _, c := range theatrical[40:] {
		extended = append(extended, Cue{Start: c.Start + shift, End: c.End + shift, Text: c.Text})
	}
	if _, ok := FitTiming(theatrical, extended); ok {
		t.Fatal("inserted scene accepted as a single offset")
	}
}

func TestSourceKeyNeverPutsURLsInPaths(t *testing.T) {
	if SourceKey("") != "" {
		t.Fatal("empty")
	}
	if SourceKey("local:abc") != "local_abc" {
		t.Fatalf("local: %q", SourceKey("local:abc"))
	}
	u := "http://127.0.0.1:32400/src/sess/tok"
	k := SourceKey(u)
	if k == "" || strings.ContainsAny(k, "/:\\") {
		t.Fatalf("url key %q", k)
	}
	if SourceKey(u) != k {
		t.Fatal("unstable")
	}
}

func TestOpenFormatFromUsesSourceKey(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	id := "local-en"
	raw := []byte(RenderSRT(synthCues(8, time.Second, 1500*time.Millisecond, 1)))
	if err := os.WriteFile(filepath.Join(s.dir, id+".srt"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s.storeCorrection(id, SourceKey("local:rel"), correction{
		Offset: -1500 * time.Millisecond, Rate: 1, Applied: true, Measured: true, At: s.now().UTC(),
	})
	r, _, err := s.OpenFormatFrom(id, "srt", "local:rel")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	cues, err := ParseCues(string(got))
	if err != nil || len(cues) == 0 {
		t.Fatalf("parse %v", err)
	}
	if absDuration(cues[0].Start) > timingFrameSlack {
		t.Fatalf("first cue %s", cues[0].Start)
	}
}

func TestOpenFormatFromWaitsForInFlight(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	id := "wait-en"
	raw := []byte(RenderSRT(synthCues(8, time.Second, 1500*time.Millisecond, 1)))
	if err := os.WriteFile(filepath.Join(s.dir, id+".srt"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	src := SourceKey("rel-w")
	s.correctFlights.run(id+"\x00"+src, func() {
		time.Sleep(200 * time.Millisecond)
		s.storeCorrection(id, src, correction{
			Offset: -1500 * time.Millisecond, Rate: 1, Applied: true, Measured: true, At: s.now().UTC(),
		})
	})
	r, _, err := s.OpenFormatFrom(id, "srt", "rel-w")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	cues, err := ParseCues(string(got))
	if err != nil || len(cues) == 0 || absDuration(cues[0].Start) > timingFrameSlack {
		t.Fatalf("did not wait for correction: %v %v", err, cues)
	}
}

func TestSyncHandlerManualAdjust(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	id := "manual-en"
	raw := []byte(RenderSRT(synthCues(8, time.Second, 0, 1)))
	if err := os.WriteFile(filepath.Join(s.dir, id+".srt"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/"+id, strings.NewReader(`{"offsetMs":1500,"rate":1,"apply":true}`))
	rec := httptest.NewRecorder()
	s.SyncHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.Bytes())
	}
	r, _, err := s.OpenFormatFrom(id, "srt", "other-release")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	cues, err := ParseCues(string(got))
	if err != nil || len(cues) == 0 || absDuration(cues[0].Start-1500*time.Millisecond) > timingFrameSlack {
		t.Fatalf("manual adjust not applied: %v %v", err, cues)
	}
}

func TestManualAdjustHonoursApplyAndWins(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	id := "manual-win"
	raw := []byte(RenderSRT(synthCues(8, time.Second, 0, 1)))
	if err := os.WriteFile(filepath.Join(s.dir, id+".srt"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s.storeCorrection(id, SourceKey("rel"), correction{
		Offset: 9 * time.Second, Rate: 1, Applied: true, Measured: true, At: s.now().UTC(),
	})
	req := httptest.NewRequest(http.MethodPost, "/"+id, strings.NewReader(`{"offsetMs":1500,"rate":1,"apply":false}`))
	rec := httptest.NewRecorder()
	s.SyncHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.Bytes())
	}
	r, _, err := s.OpenFormatFrom(id, "srt", "rel")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	cues, err := ParseCues(string(got))
	if err != nil || len(cues) == 0 || absDuration(cues[0].Start-9*time.Second) > timingFrameSlack {
		t.Fatalf("apply=false should leave measured: %v %v", err, cues)
	}

	req = httptest.NewRequest(http.MethodPost, "/"+id, strings.NewReader(`{"offsetMs":1500,"rate":1,"apply":true}`))
	rec = httptest.NewRecorder()
	s.SyncHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("apply status %d %s", rec.Code, rec.Body.Bytes())
	}
	r, _, err = s.OpenFormatFrom(id, "srt", "rel")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(r)
	r.Close()
	cues, err = ParseCues(string(got))
	if err != nil || len(cues) == 0 || absDuration(cues[0].Start-1500*time.Millisecond) > timingFrameSlack {
		t.Fatalf("manual should win: %v %v", err, cues)
	}
}

func TestManualAdjustRejectsBoundsAndTraversal(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	id := "manual-bound"
	if err := os.WriteFile(filepath.Join(s.dir, id+".srt"), []byte(RenderSRT(synthCues(4, time.Second, 0, 1))), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"/" + id, `{"offsetMs":3600001,"rate":1,"apply":true}`, 400},
		{"/" + id, `{"offsetMs":0,"rate":3,"apply":true}`, 400},
		{"/" + id, `{"offsetMs":0,"rate":0.4,"apply":true}`, 400},
		{"/..", `{"offsetMs":0,"rate":1,"apply":true}`, 400},
	} {
		rec := httptest.NewRecorder()
		s.SyncHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.code {
			t.Fatalf("%s %s: status %d want %d %s", tc.path, tc.body, rec.Code, tc.code, rec.Body.Bytes())
		}
	}
}

func TestManualAdjustAcceptsASSReference(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	id := "manual-ass"
	if err := os.WriteFile(filepath.Join(s.dir, id+".srt"), []byte(RenderSRT(synthCues(4, time.Second, 0, 1))), 0o644); err != nil {
		t.Fatal(err)
	}
	ass := "[Script Info]\nTitle: t\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,One\nDialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,Two\nDialogue: 0,0:00:05.00,0:00:06.00,Default,,0,0,0,,Three\n"
	body, err := json.Marshal(map[string]any{"offsetMs": 0, "rate": 1, "apply": true, "reference": ass})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.SyncHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+id, strings.NewReader(string(body))))
	if rec.Code != 200 {
		t.Fatalf("ASS reference rejected: %d %s", rec.Code, rec.Body.Bytes())
	}
}

func TestStartCorrectionSkipsWithoutReleaseKey(t *testing.T) {
	s := New(Options{CacheDir: t.TempDir()})
	held := false
	s.StartCorrection(context.Background(), &core.Title{}, "sid", ReleaseAccess{
		URL: "http://127.0.0.1/x",
		Hold: func() (io.ReaderAt, int64, func(), bool) {
			held = true
			return nil, 0, nil, false
		},
	})
	time.Sleep(20 * time.Millisecond)
	if held {
		t.Fatal("held without a release key")
	}
}

type metaRA struct {
	meta, live int
}

func (m *metaRA) ReadAt(p []byte, _ int64) (int, error) {
	m.live++
	return copy(p, []byte("live")), nil
}

func (m *metaRA) ReadMetadataAtContext(_ context.Context, p []byte, _ int64) (int, error) {
	m.meta++
	return copy(p, []byte("meta")), nil
}

func TestLimitReaderAtPassesMetadataReads(t *testing.T) {
	inner := &metaRA{}
	r := limitReaderAt(inner, 32)
	mr, ok := r.(core.MetadataReaderAt)
	if !ok {
		t.Fatal("limitReaderAt hid ReadMetadataAtContext")
	}
	buf := make([]byte, 4)
	n, err := mr.ReadMetadataAtContext(context.Background(), buf, 0)
	if err != nil || n != 4 || string(buf) != "meta" || inner.meta != 1 || inner.live != 0 {
		t.Fatalf("n=%d err=%v buf=%q meta=%d live=%d", n, err, buf, inner.meta, inner.live)
	}
}
