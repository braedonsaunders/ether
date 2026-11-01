package livetv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestVirtualDASHJoinsAtOffsetRollsOverAndCloses(t *testing.T) {
	now := time.Unix(7, 0).UTC()
	pb := &fakePlayback{}
	tc := &fakeTranscoder{}
	s := testService(t, Options{Now: func() time.Time { return now }, Playback: pb, Transcoder: tc})
	c := testChannel()
	c.Items = []Item{
		{Key: core.MovieKey(1), Title: "First", Rating: "G", DurationMS: 12000},
		{Key: core.MovieKey(2), Title: "Second", Rating: "PG", DurationMS: 12000},
		{Key: core.MovieKey(3), Title: "Third", Rating: "G", DurationMS: 12000},
	}
	if _, err := s.SaveChannel(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	info, err := s.Tune(context.Background(), u, c.ID, core.ClientCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.requests) != 1 || tc.requests[0].Protocol == core.ProtocolDASH || tc.requests[0].LiveInput {
		t.Fatalf("tune starts one HLS session, got %+v", tc.requests)
	}

	req := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd?path=/livetv/sessions/"+info.ID, nil)
	rec := httptest.NewRecorder()
	if err = s.ServeDASH(rec, req, info.ID, u); err != nil {
		t.Fatal(err)
	}
	manifest := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(manifest, `type="dynamic"`) || strings.Contains(manifest, "mediaPresentationDuration") {
		t.Fatalf("live manifest: %d %s", rec.Code, manifest)
	}
	for _, want := range []string{
		`profiles="urn:mpeg:dash:profile:isoff-live:2011"`,
		`minimumUpdatePeriod="PT2S"`,
		`timeShiftBufferDepth="PT120S"`,
		`suggestedPresentationDelay="PT8S"`,
		`minBufferTime="PT4S"`,
		`<SegmentTimeline>`,
		"/livetv/sessions/" + info.ID + "/0/dash/",
		"X-Ether-Stream-Token=",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest missing %s\n%s", want, manifest)
		}
	}
	if strings.Contains(manifest, "X-Plex-Token") {
		t.Fatalf("account token in DASH manifest:\n%s", manifest)
	}

	var joined *core.HLSRequest
	for i := range tc.requests {
		r := &tc.requests[i]
		if r.Protocol != core.ProtocolDASH {
			continue
		}
		if r.LiveInput {
			t.Fatal("virtual DASH must not open the input as a live playlist")
		}
		if strings.HasSuffix(r.SessionID, "-"+strconv.FormatInt(info.Programme.Start.UnixMilli(), 10)) {
			joined = r
		}
	}
	if joined == nil || joined.StartOffset != 4*time.Second || joined.Info.Duration != 12*time.Second {
		t.Fatalf("join seek wrong: %+v", joined)
	}
	var rolled *core.HLSRequest
	for i := range tc.requests {
		r := &tc.requests[i]
		if r.Protocol == core.ProtocolDASH && strings.HasSuffix(r.SessionID, "-"+strconv.FormatInt(info.Programme.Start.Add(12*time.Second).UnixMilli(), 10)) {
			rolled = r
		}
	}
	if rolled == nil || rolled.StartOffset != 0 || rolled.Info.Duration != 12*time.Second {
		t.Fatalf("next programme did not start at its beginning: %+v", rolled)
	}

	part := httptest.NewRecorder()
	s.ServeDASHPart(part, req, info.ID, u, 0, 0, false)
	if part.Code != 200 || !strings.Contains(part.Body.String(), "media:") || !strings.HasSuffix(part.Body.String(), ":0:1") {
		t.Fatalf("join segment: %d %s", part.Code, part.Body.String())
	}
	init := httptest.NewRecorder()
	s.ServeDASHPart(init, req, info.ID, u, 0, 0, true)
	if init.Code != 200 || !strings.Contains(init.Body.String(), "init:") {
		t.Fatalf("init: %d %s", init.Code, init.Body.String())
	}
	next := httptest.NewRecorder()
	s.ServeDASHPart(next, req, info.ID, u, 0, 2, false)
	if next.Code != 200 || !strings.Contains(next.Body.String(), "-"+strconv.FormatInt(info.Programme.Start.Add(12*time.Second).UnixMilli(), 10)+":") || !strings.HasSuffix(next.Body.String(), ":0:0") {
		t.Fatalf("rollover segment: %d %s", next.Code, next.Body.String())
	}

	now = time.Unix(40, 0).UTC()
	again := httptest.NewRecorder()
	if err = s.ServeDASH(again, req, info.ID, u); err != nil || again.Code != 200 || !strings.Contains(again.Body.String(), `type="dynamic"`) {
		t.Fatalf("manifest after rollover: %v %d %s", err, again.Code, again.Body.String())
	}
	if !tc.dash[0].closed {
		t.Fatal("programme that fell out of the live window kept its transcoder")
	}

	if err = s.Stop(context.Background(), info.ID, u); err != nil {
		t.Fatal(err)
	}
	for _, h := range tc.hls {
		if !h.closed {
			t.Fatalf("transcoder %s left open", h.id)
		}
	}
	if len(pb.stopped) < len(pb.prepared) {
		t.Fatalf("playback sessions leaked: prepared %d stopped %v", len(pb.prepared), pb.stopped)
	}
	if len(s.ViewingSessions()) != 0 {
		t.Fatal("live session leaked")
	}
}

func TestVirtualDASHFailureIsNotABare503(t *testing.T) {
	pb := &fakePlayback{}
	tc := &hlsOnlyTranscoder{fakeTranscoder: &fakeTranscoder{}}
	s := testService(t, Options{Now: func() time.Time { return time.Unix(7, 0).UTC() }, Playback: pb, Transcoder: tc})
	c := testChannel()
	if _, err := s.SaveChannel(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	info, err := s.Tune(context.Background(), u, c.ID, core.ClientCaps{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	err = s.ServeDASH(rec, httptest.NewRequest(http.MethodGet, "/", nil), info.ID, u)
	var dashErr *VirtualDASHError
	if !errors.As(err, &dashErr) || rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("virtual DASH failure wrote a response (%d %q) or lost the error: %v", rec.Code, rec.Body.String(), err)
	}
	if dashErr.Message == "" {
		t.Fatal("missing client-facing DASH failure")
	}
	for _, h := range tc.hls {
		if strings.Contains(h.id, "livetv-dash-") && !h.closed {
			t.Fatal("rejected DASH session was left open")
		}
	}
}

func TestVirtualLiveMPDBufferIsOneSegment(t *testing.T) {
	origin := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	reps := []virtualRep{{id: 0, kind: "video", codecs: "avc1.640028", bandwidth: 4000000}}
	early := renderVirtualLiveMPD(origin, origin.Add(10*time.Second), 0, []dashPiece{{seq: 0, at: origin, dur: segmentDuration}}, "i", "m", reps)
	if !strings.Contains(early, `minBufferTime="PT4S"`) || !strings.Contains(early, `timeShiftBufferDepth="PT120S"`) {
		t.Fatalf("live MPD must keep PT120S rewind and one-segment start buffer: %s", early)
	}
}

type hlsOnlyTranscoder struct{ *fakeTranscoder }

func (f *hlsOnlyTranscoder) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	if r.Protocol == core.ProtocolDASH {
		f.requests = append(f.requests, r)
		h := &fakeHLS{id: r.SessionID}
		f.hls = append(f.hls, h)
		return h, nil
	}
	return f.fakeTranscoder.StartHLS(ctx, r)
}
