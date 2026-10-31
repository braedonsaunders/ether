package plex

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

func countTracks(streams []model.Stream) (audio, subs, selAudio int) {
	for _, st := range streams {
		switch st.StreamType {
		case 2:
			audio++
			if st.Selected != nil && *st.Selected {
				selAudio++
			}
		case 3:
			subs++
		}
	}
	return audio, subs, selAudio
}

// warmProbePlayback serves a completed warm-up probe (the file's real
// tracks) through the core.WarmProber seam, over the fixture's estimate.
type warmProbePlayback struct {
	core.Playback
	info core.MediaInfo
	ok   bool
}

func (w *warmProbePlayback) WarmProbeInfo(*core.User, core.ClientCaps, core.RatingKey) (core.MediaInfo, bool) {
	return w.info, w.ok
}

// First open, no session and no resume row: detail/play-queue metadata
// must list the warm probe's real tracks once a detail-page warm-up has
// probed the file. ResumedTracks covers later opens; this covers the
// first one. Without warm facts the estimate stands.
func TestMetadataListsWarmProbedTracks(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()

	pr := &Request{User: &ownerUser, ClientID: "client-1"}

	info, ps := f.srv.mediaInfoFor(context.Background(), pr, &fightClub)
	if ps != nil || !info.Estimated {
		t.Fatalf("no warm facts: ps=%v estimated=%v, want nil/true", ps != nil, info.Estimated)
	}

	probed := sampleInfo
	probed.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
	f.srv.d.Playback = &warmProbePlayback{Playback: f.playback, info: probed, ok: true}
	info, ps = f.srv.mediaInfoFor(context.Background(), pr, &fightClub)
	if ps != nil || info.Estimated {
		t.Fatalf("warm probe: ps=%v estimated=%v, want nil/false", ps != nil, info.Estimated)
	}
	md := f.srv.metadata(context.Background(), pr, &fightClub, renderOpts{detail: true, media: true})
	audio, subs, selAudio := countTracks(md.Media[0].Part[0].Stream)
	if audio != 2 || subs != 2 || selAudio != 1 {
		t.Errorf("warm metadata: audio=%d subs=%d selAudio=%d, want 2/2/1", audio, subs, selAudio)
	}
	for _, st := range md.Media[0].Part[0].Stream {
		if st.Decision != "" || st.Location != "" || strings.TrimSpace(st.DisplayTitle) == "" {
			t.Errorf("metadata stream %d decision=%q location=%q, want detail shape (both omitted)", st.ID, st.Decision, st.Location)
		}
	}
}

// When Variants (the "Play Version" picker) is wired — as it always is in
// production — a completed warm probe must also populate the Auto variant's
// Stream list and Media attributes on the detail page, instead of being
// overwritten by the variant resolver's one-audio estimate.
func TestMetadataWithVariantsListsWarmProbedTracks(t *testing.T) {
	probed := sampleInfo
	probed.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
	pb := &warmProbePlayback{
		Playback: &fakeVariantPlayback{fakePlayback: newFakePlayback(sampleInfo)},
		info:     probed,
		ok:       true,
	}
	f := newVariantFixture(t, pb)
	pr := &Request{User: &ownerUser, ClientID: "client-1", Product: "Plex for Android (TV)", Platform: "Android", Device: "SHIELD Android TV"}

	md := f.srv.metadata(context.Background(), pr, &fightClub, renderOpts{detail: true, media: true})
	if len(md.Media) != 3 {
		t.Fatalf("media count = %d, want 3 variants", len(md.Media))
	}
	audio, subs, selAudio := countTracks(md.Media[0].Part[0].Stream)
	if audio != 2 || subs != 2 || selAudio != 1 {
		t.Fatalf("Auto variant warm streams: audio=%d subs=%d selAudio=%d, want 2/2/1", audio, subs, selAudio)
	}
	for i := 1; i < len(md.Media); i++ {
		if len(md.Media[i].Part[0].Stream) != 0 {
			t.Errorf("non-Auto variant %d should not carry warm streams before playback: %+v", i, md.Media[i].Part[0].Stream)
		}
	}
}

// A non-owner detail render starts the warm-up and then asks for media
// before that warm-up has published. The render waits for the probe instead
// of painting the 1080p estimate.
type lateWarmPlayback struct {
	core.Playback
	info    core.MediaInfo
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (w *lateWarmPlayback) WarmProbeInfo(*core.User, core.ClientCaps, core.RatingKey) (core.MediaInfo, bool) {
	return core.MediaInfo{}, false
}

func (w *lateWarmPlayback) AwaitWarmProbe(ctx context.Context, _ *core.User, _ core.ClientCaps, _ core.RatingKey) (core.MediaInfo, bool) {
	w.once.Do(func() { close(w.started) })
	select {
	case <-w.release:
		return w.info, true
	case <-ctx.Done():
		return core.MediaInfo{}, false
	}
}

func TestMetadataWaitsForInFlightWarmProbe(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	probed := core.MediaInfo{
		Container: "mp4", Duration: 20 * time.Second, BitrateKbps: 17,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 320, Height: 180},
			{Index: 1, Type: "audio", Codec: "aac", Language: "fr", Channels: 2},
			{Index: 2, Type: "audio", Codec: "aac", Language: "en", Channels: 2},
			{Index: 3, Type: "subtitle", Codec: "mov_text", Language: "en"},
		},
	}
	late := &lateWarmPlayback{
		Playback: f.playback,
		info:     probed,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	f.srv.d.Playback = late
	pr := &Request{User: &core.User{ID: 9, Username: "adult"}, ClientID: "adult-androidtv"}
	got := make(chan core.MediaInfo, 1)
	go func() {
		info, _ := f.srv.mediaInfoFor(context.Background(), pr, &fightClub)
		got <- info
	}()
	select {
	case <-late.started:
	case <-time.After(time.Second):
		t.Fatal("detail render did not wait for the in-flight probe")
	}
	select {
	case info := <-got:
		t.Fatalf("render returned the estimate before the probe: estimated=%v container=%s", info.Estimated, info.Container)
	case <-time.After(40 * time.Millisecond):
	}
	close(late.release)
	select {
	case info := <-got:
		if info.Estimated || info.Container != "mp4" || info.Duration != 20*time.Second || len(info.Streams) != 4 || info.Streams[0].Width != 320 {
			t.Fatalf("probed media = %+v", info)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out after the probe was published")
	}
}
