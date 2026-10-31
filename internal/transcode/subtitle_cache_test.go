package transcode

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingSubtitleRunner struct {
	Runner
	runs    atomic.Int32
	release chan struct{}
}

func (r *countingSubtitleRunner) Run(ctx context.Context, c Command) error {
	r.runs.Add(1)
	if r.release != nil {
		<-r.release
	}
	_, err := io.WriteString(c.Stdout, "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nhello\n")
	return err
}

// An embedded track is extracted once per source and track: later and
// concurrent requests for it, from any session of that source, share it.
func TestSubtitleExtractedOncePerSource(t *testing.T) {
	r := &countingSubtitleRunner{release: make(chan struct{})}
	m := &Manager{runner: r, ffmpeg: "ffmpeg"}
	ctx := WithSubtitleSource(context.Background(), "release-1")
	var wg sync.WaitGroup
	outs := make([]strings.Builder, 4)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.ExtractSubtitle(ctx, "http://h/session-"+string(rune('a'+i)), 3, "subrip", &outs[i]); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(r.release)
	wg.Wait()
	var again strings.Builder
	if err := m.ExtractSubtitle(ctx, "http://h/another-session", 3, "subrip", &again); err != nil {
		t.Fatal(err)
	}
	if n := r.runs.Load(); n != 1 {
		t.Fatalf("extracted %d times", n)
	}
	for i := range outs {
		if !strings.Contains(outs[i].String(), "hello") {
			t.Fatalf("request %d: %q", i, outs[i].String())
		}
	}
	if !strings.Contains(again.String(), "hello") {
		t.Fatalf("cached: %q", again.String())
	}
	// Another track, or no source name, extracts.
	r.release = nil
	_ = m.ExtractSubtitle(ctx, "http://h/x", 4, "subrip", io.Discard)
	_ = m.ExtractSubtitle(context.Background(), "http://h/x", 3, "subrip", io.Discard)
	if n := r.runs.Load(); n != 3 {
		t.Fatalf("extracted %d times, want 3", n)
	}
}
