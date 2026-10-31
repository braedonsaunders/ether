package plex

import (
	"context"
	"strings"
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

// warmWaiterPlayback simulates a cold detail-page open where Prewarm kicks
// off a background resolve+probe and WaitWarmProbe completes within the
// bounded detail budget so the very first GET /library/metadata/{key}
// returns real audio and subtitle streams.
type warmWaiterPlayback struct {
	core.Playback
	info      core.MediaInfo
	warmReady map[core.RatingKey]bool
	prewarms  []core.RatingKey
	waits     []core.RatingKey
}

func (w *warmWaiterPlayback) Prewarm(_ context.Context, _ *core.User, k core.RatingKey, _ core.ClientCaps, _ core.PrewarmOptions) bool {
	w.prewarms = append(w.prewarms, k)
	return true
}

func (w *warmWaiterPlayback) WaitWarmProbe(_ context.Context, _ *core.User, _ core.ClientCaps, k core.RatingKey, _ time.Duration) (core.MediaInfo, bool) {
	w.waits = append(w.waits, k)
	if w.warmReady == nil {
		w.warmReady = map[core.RatingKey]bool{}
	}
	w.warmReady[k] = true
	return w.info, true
}

func (w *warmWaiterPlayback) WarmProbeInfo(_ *core.User, _ core.ClientCaps, k core.RatingKey) (core.MediaInfo, bool) {
	if w.warmReady[k] {
		return w.info, true
	}
	return core.MediaInfo{}, false
}

func TestFirstOpenMovieAndShowSeasonWarmProbedTracks(t *testing.T) {
	probed := sampleInfo
	probed.Streams = append([]core.StreamInfo(nil), sampleInfo.Streams...)
	f := newFixture()
	defer f.srv.Close()
	wpb := &warmWaiterPlayback{Playback: f.playback, info: probed}
	f.srv.d.Playback = wpb

	// 1. Cold first open of a movie detail page waits for the warm probe and renders real tracks.
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String(),
		"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Client-Identifier", "shield-1")
	if rec.Code != 200 {
		t.Fatalf("movie metadata %d: %s", rec.Code, rec.Body.String())
	}
	if len(audioTags(rec.Body.String())) < 2 || len(streamTagsOfType(rec.Body.String(), "3")) < 2 {
		t.Fatalf("first movie open did not include warm probed audio/subtitle tracks:\n%s", rec.Body.String())
	}

	// 2. Show detail page with includeOnDeck=1 warms the next episode (bbE1) and includes its real streams in OnDeck.
	showRec := f.get(t, "/library/metadata/"+breakingBad.Key.String()+"?includeOnDeck=1",
		"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Client-Identifier", "shield-1")
	if showRec.Code != 200 {
		t.Fatalf("show metadata %d: %s", showRec.Code, showRec.Body.String())
	}
	if len(audioTags(showRec.Body.String())) < 2 || len(streamTagsOfType(showRec.Body.String(), "3")) < 2 {
		t.Fatalf("show OnDeck did not include warm probed tracks for next episode:\n%s", showRec.Body.String())
	}

	// 3. Season children page warms the next episode and renders real streams on the warmed episode.
	wpb.warmReady = map[core.RatingKey]bool{}
	childrenRec := f.get(t, "/library/metadata/"+bbS1.Key.String()+"/children",
		"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Client-Identifier", "shield-1")
	if childrenRec.Code != 200 {
		t.Fatalf("season children %d: %s", childrenRec.Code, childrenRec.Body.String())
	}
	if len(audioTags(childrenRec.Body.String())) < 2 || len(streamTagsOfType(childrenRec.Body.String(), "3")) < 2 {
		t.Fatalf("season children did not include warm probed tracks for next episode:\n%s", childrenRec.Body.String())
	}
}
