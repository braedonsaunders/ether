package transcode

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPreviewRejectsPublicationAfterSourceReplacement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var current atomic.Bool
	current.Store(true)
	runner := newFakeRunner()
	runner.runFunc = func(ctx context.Context, cmd Command) error {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := cmd.Stdout.Write(fakeJPEG(1, 64))
		return err
	}
	thumbs := NewThumbnails(ThumbOptions{Runner: runner, FFmpeg: "ffmpeg", BatchPause: time.Millisecond})
	defer thumbs.Close()
	thumbs.EnsureTrickplay(context.Background(), core.TrickplaySource{ReleaseID: "member-revision", InputURL: "http://source/bytes", Info: core.MediaInfo{Duration: time.Second}, AvailableUntil: time.Second, Current: current.Load})
	<-entered
	current.Store(false)
	close(release)
	waitThumbFor(t, "retired source job", func() bool { return thumbs.isIdle("member-revision") })
	if thumbs.frameCount("member-revision") != 0 {
		t.Fatal("retired source published a preview")
	}
}
