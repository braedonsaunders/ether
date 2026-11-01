package subtitles

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func synthCues(n int, step, offset time.Duration, rate float64) []Cue {
	out := make([]Cue, n)
	for i := 0; i < n; i++ {
		start := time.Duration(float64(time.Duration(i)*step)*rate) + offset
		out[i] = Cue{Start: start, End: start + 800*time.Millisecond, Text: "x"}
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
	ref := synthCues(1200, 2*time.Second, 0, 1)
	cand := synthCues(1200, 2*time.Second, 1800*time.Millisecond, 1)
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
	ref := synthCues(40, 2*time.Second, 0, 1)
	fit, ok := FitTiming(ref, ref)
	if !ok || !fit.Identity() {
		t.Fatalf("in-sync %+v ok=%v", fit, ok)
	}
}

func TestFitTimingRejectsDifferentCut(t *testing.T) {
	ref := synthCues(40, 2*time.Second, 0, 1)
	cand := synthCues(40, 3500*time.Millisecond, 45*time.Second, 1.12)
	if _, ok := FitTiming(cand, ref); ok {
		t.Fatal("different cut accepted")
	}
}
