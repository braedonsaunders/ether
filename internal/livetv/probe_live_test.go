package livetv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type fixtureLiveProber struct {
	core.Transcoder
	calls int
	err   error
}

func (p *fixtureLiveProber) ProbeLive(context.Context, string) (*core.MediaInfo, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &core.MediaInfo{Streams: []core.StreamInfo{{Type: "video", Codec: "h264", Width: 1280, Height: 720}}}, nil
}

func TestLiveCodecCacheExpiresAndDoesNotCacheFailures(t *testing.T) {
	now := time.Unix(100, 0)
	p := &fixtureLiveProber{}
	s := testService(t, Options{Transcoder: p, Now: func() time.Time { return now }})
	for i := 0; i < 2; i++ {
		info, err := s.probeLiveInput(context.Background(), "https://fixture.invalid/channel")
		if err != nil || info.Video() == nil {
			t.Fatal(err)
		}
		info.Duration = time.Hour // caller changes must not alter the cache
	}
	if p.calls != 1 {
		t.Fatal("retune repeated the expensive live probe")
	}
	now = now.Add(liveProbeTTL + time.Second)
	s.probeLiveInput(context.Background(), "https://fixture.invalid/channel")
	if p.calls != 2 {
		t.Fatal("expired live metadata was reused")
	}
	p.err = errors.New("temporary upstream failure")
	for i := 0; i < 2; i++ {
		if _, err := s.probeLiveInput(context.Background(), "https://fixture.invalid/other"); err == nil {
			t.Fatal("failed probe succeeded")
		}
	}
	if p.calls != 4 {
		t.Fatal("failed analysis was cached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.probeLiveInput(ctx, "https://fixture.invalid/channel"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled tune ignored cancellation")
	}
}

func TestLiveProbeSingleflight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	p := &blockingProber{entered: entered, release: release}
	s := testService(t, Options{Transcoder: p, Now: func() time.Time { return time.Unix(100, 0) }})
	var wg sync.WaitGroup
	errc := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.probeLiveInput(context.Background(), "https://fixture.invalid/channel")
			errc <- err
		}()
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not start")
	}
	if p.calls != 1 {
		t.Fatalf("concurrent tunes probed %d times", p.calls)
	}
	close(release)
	wg.Wait()
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	if p.calls != 1 {
		t.Fatalf("shared probe was repeated: %d", p.calls)
	}
}

type blockingProber struct {
	fixtureLiveProber
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProber) ProbeLive(ctx context.Context, input string) (*core.MediaInfo, error) {
	p.calls++
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &core.MediaInfo{Streams: []core.StreamInfo{{Type: "video", Codec: "h264", Width: 1280, Height: 720}}}, nil
}

func TestIPTVProbeOverlapsThePlaylistPrefix(t *testing.T) {
	var inPrefix atomic.Bool
	var overlapped atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inPrefix.Store(true)
		defer inPrefix.Store(false)
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nsegment.ts\n")
	}))
	defer up.Close()
	prober := &overlapProber{fakeTranscoder: &fakeTranscoder{}, inPrefix: &inPrefix, overlapped: &overlapped}
	s := testService(t, Options{Transcoder: prober, HTTPClient: up.Client(), Now: func() time.Time { return time.Unix(100, 0) }})
	saved, err := s.SaveChannel(context.Background(), Channel{Name: "News", Number: "4", Kind: "iptv", Enabled: true, StreamURL: up.URL + "/live"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Tune(context.Background(), &core.User{ID: 1}, saved.ID, core.ClientCaps{VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}); err != nil {
		t.Fatal(err)
	}
	if !overlapped.Load() {
		t.Fatal("codec probe waited until the playlist prefix finished")
	}
}

type overlapProber struct {
	*fakeTranscoder
	inPrefix   *atomic.Bool
	overlapped *atomic.Bool
}

func (p *overlapProber) ProbeLive(context.Context, string) (*core.MediaInfo, error) {
	time.Sleep(40 * time.Millisecond)
	if p.inPrefix.Load() {
		p.overlapped.Store(true)
	}
	return &core.MediaInfo{Streams: []core.StreamInfo{{Type: "video", Codec: "h264", Width: 1280, Height: 720}, {Type: "audio", Codec: "aac"}}}, nil
}
