package plex

import (
	"context"
	"strings"
	"testing"

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
