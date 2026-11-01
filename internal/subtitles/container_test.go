package subtitles

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestReadEmbeddedTextCuesMKVSeekHeadAndManyClusters(t *testing.T) {
	TestReadEmbeddedTextCuesMKV(t)
}

func TestReadEmbeddedTextCuesMKV(t *testing.T) {
	ref := synthCues(12, 2*time.Second, 0, 1)
	for i := range ref {
		ref[i].Text = "line"
	}
	raw := buildTestMKV(ref)
	got, err := ReadEmbeddedTextCues(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 10 {
		t.Fatalf("cues %d", len(got))
	}
	if absDuration(got[3].Start-ref[3].Start) > timingFrameSlack {
		t.Fatalf("cue 3 %s vs %s", got[3].Start, ref[3].Start)
	}
	cand := synthCues(12, 2*time.Second, 1500*time.Millisecond, 1)
	fit, ok := FitTiming(cand, got)
	if !ok || absDuration(fit.Offset+1500*time.Millisecond) > timingFrameSlack {
		t.Fatalf("fit %+v ok=%v", fit, ok)
	}
}

func TestReadEmbeddedTextCuesMP4(t *testing.T) {
	ref := synthCues(12, 2*time.Second, 0, 1)
	for i := range ref {
		ref[i].Text = "line"
	}
	raw := buildTestMP4(ref)
	got, err := ReadEmbeddedTextCues(context.Background(), bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 10 {
		t.Fatalf("cues %d", len(got))
	}
	cand := synthCues(12, 2*time.Second, 0, fps25/fps23976)
	fit, ok := FitTiming(cand, got)
	if !ok {
		t.Fatal("no fit")
	}
	shifted, err := Shift(cand, fit.Offset, fit.Rate)
	if err != nil {
		t.Fatal(err)
	}
	if absDuration(shifted[4].Start-got[4].Start) > timingFrameSlack {
		t.Fatalf("cue 4 %s vs %s (rate %v off %s)", shifted[4].Start, got[4].Start, fit.Rate, fit.Offset)
	}
}

func TestReadEmbeddedTextCuesManyCues(t *testing.T) {
	ref := synthCues(1200, 2*time.Second, 0, 1)
	for i := range ref {
		ref[i].Text = "line"
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"mkv", buildTestMKV(ref)},
		{"mp4", buildTestMP4(ref)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadEmbeddedTextCues(context.Background(), bytes.NewReader(tc.raw), int64(len(tc.raw)))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) < 1000 {
				t.Fatalf("cues %d", len(got))
			}
			if absDuration(got[999].Start-ref[999].Start) > 50*time.Millisecond {
				t.Fatalf("cue 999 %s vs %s", got[999].Start, ref[999].Start)
			}
		})
	}
}
