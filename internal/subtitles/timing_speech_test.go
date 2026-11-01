package subtitles

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func toneWindow(start, span time.Duration, speech [][2]time.Duration) SpeechWindow {
	rate := 16000
	n := int(span / time.Second * time.Duration(rate))
	pcm := make([]int16, n)
	for _, s := range speech {
		a := int((s[0] - start) / time.Second * time.Duration(rate))
		b := int((s[1] - start) / time.Second * time.Duration(rate))
		if a < 0 {
			a = 0
		}
		if b > n {
			b = n
		}
		for i := a; i < b; i++ {
			pcm[i] = int16((i%8)*4000 - 16000)
		}
	}
	return SpeechWindow{Start: start, Rate: rate, PCM: pcm}
}

func TestFitSpeechEnergySynthetic(t *testing.T) {
	on := [][2]time.Duration{{time.Second, 2 * time.Second}, {4 * time.Second, 5 * time.Second}, {7 * time.Second, 8 * time.Second}}
	cues := []Cue{
		{Start: 1500 * time.Millisecond, End: 2500 * time.Millisecond, Text: "a"},
		{Start: 4500 * time.Millisecond, End: 5500 * time.Millisecond, Text: "b"},
		{Start: 7500 * time.Millisecond, End: 8500 * time.Millisecond, Text: "c"},
	}
	win := toneWindow(0, 10*time.Second, on)
	fit, ok := FitSpeechEnergyOptions(cues, []SpeechWindow{win, win}, FitOptions{MinInliers: 2, MinConfidence: 0.2, MaxOffset: 3 * time.Second})
	if !ok {
		t.Fatal("no fit")
	}
	if absDuration(fit.Offset+500*time.Millisecond) > 80*time.Millisecond {
		t.Fatalf("offset %s", fit.Offset)
	}
}

func TestFitSpeechEnergyFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "speech.wav")
	// 8 s of silence with three 1 s tone bursts at 1s, 4s, 7s.
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=800:sample_rate=16000:duration=8",
		"-af", "volume=enable='between(t,1,2)+between(t,4,5)+between(t,7,8)':volume=1,volume=enable='not(between(t,1,2)+between(t,4,5)+between(t,7,8))':volume=0.02",
		wav)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg make wav: %v %s", err, out)
	}
	if _, err := os.Stat(wav); err != nil {
		t.Fatal(err)
	}
	cues := []Cue{
		{Start: 1500 * time.Millisecond, End: 2500 * time.Millisecond, Text: "a"},
		{Start: 4500 * time.Millisecond, End: 5500 * time.Millisecond, Text: "b"},
		{Start: 7500 * time.Millisecond, End: 8500 * time.Millisecond, Text: "c"},
	}
	windows, err := DecodeAudioWindows(t.Context(), "ffmpeg", wav, 8*time.Second, 3, 3*time.Second)
	if err != nil || len(windows) == 0 {
		t.Fatalf("decode %v %d", err, len(windows))
	}
	fit, ok := FitSpeechEnergyOptions(cues, windows, FitOptions{MinInliers: 1, MinConfidence: 0.15, MaxOffset: 3 * time.Second})
	if !ok {
		t.Fatal("no fit")
	}
	if absDuration(fit.Offset+500*time.Millisecond) > 200*time.Millisecond {
		t.Fatalf("offset %s", fit.Offset)
	}
}
