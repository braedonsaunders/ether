package transcode

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Issue #434: LG webOS (GStreamer qtdemux) rejected the in-GOP placeholder
// fragments #417 introduced and looped 00001->00000->00003 forever. Those
// placeholders were styp + moof(mfhd) + mdat: a moof with no traf, which
// ISO/IEC 14496-12 forbids and strict demuxers reject. The fragment must
// carry the video track's own tfhd + tfdt (base media decode time at the
// segment's start) + an empty trun, so the player's clock advances.
func TestIssue434EmptyFMP4FragmentIsConformant(t *testing.T) {
	track := emptyFMP4Tracks{TrackID: 1, Timescale: 90000}
	base := emptyFMP4DecodeTime(4*time.Second, track.Timescale)
	if base != 4*90000 {
		t.Fatalf("decode time = %d, want %d", base, 4*90000)
	}
	b := emptyFMP4(3, track, base)

	if !isEmptyFMP4(b) {
		t.Fatalf("emptyFMP4 output not recognised as an empty placeholder: %x", b)
	}
	top, err := readBoxes(b, 0, len(b))
	if err != nil || len(top) != 3 || top[0].typ != "styp" || top[1].typ != "moof" || top[2].typ != "mdat" {
		t.Fatalf("top boxes = %+v err=%v", top, err)
	}
	kids, err := readBoxes(b, top[1].start+top[1].hdr, top[1].end)
	if err != nil {
		t.Fatalf("moof children: %v", err)
	}
	var traf *mp4Box
	for i := range kids {
		if kids[i].typ == "traf" {
			traf = &kids[i]
		}
	}
	if traf == nil {
		t.Fatal("moof has no traf: a demuxer cannot advance the timeline")
	}
	grandkids, err := readBoxes(b, traf.start+traf.hdr, traf.end)
	if err != nil {
		t.Fatalf("traf children: %v", err)
	}
	var sawTfhd, sawTfdt bool
	for _, g := range grandkids {
		switch g.typ {
		case "tfhd":
			p := g.payload(b)
			if len(p) < 8 || uint32(p[4])<<24|uint32(p[5])<<16|uint32(p[6])<<8|uint32(p[7]) != track.TrackID {
				t.Fatalf("tfhd track id wrong: %x", p)
			}
			sawTfhd = true
		case "tfdt":
			p := g.payload(b)
			if len(p) < 12 || p[0] != 1 {
				t.Fatalf("tfdt not version 1: %x", p)
			}
			got := uint64(p[4])<<56 | uint64(p[5])<<48 | uint64(p[6])<<40 | uint64(p[7])<<32 |
				uint64(p[8])<<24 | uint64(p[9])<<16 | uint64(p[10])<<8 | uint64(p[11])
			if got != base {
				t.Fatalf("tfdt base = %d, want %d", got, base)
			}
			sawTfdt = true
		}
	}
	if !sawTfhd || !sawTfdt {
		t.Fatalf("traf missing tfhd=%v tfdt=%v", sawTfhd, sawTfdt)
	}

	// moofTiming (the reader used for real chunks) must accept it and place
	// it at the requested decode time, not error "moof without tfdt".
	start, end, err := moofTiming(b[top[1].start:top[1].end], 0)
	if err != nil {
		t.Fatalf("moofTiming: %v", err)
	}
	if start != int64(base) || end != int64(base) {
		t.Fatalf("moofTiming span = %d..%d, want %d..%d", start, end, base, base)
	}
}

// A real GOP segment must never be mistaken for an empty placeholder.
func TestIssue434EmptyFMP4DoesNotMatchRealFragment(t *testing.T) {
	real := []byte{
		0, 0, 0, 24, 's', 't', 'y', 'p', 'm', 's', 'd', 'h', 0, 0, 0, 0, 'm', 's', 'd', 'h', 'm', 's', 'i', 'x',
		0, 0, 0, 32, 'm', 'o', 'o', 'f', // moof (24 header + traf)
		0, 0, 0, 16, 'm', 'f', 'h', 'd', 0, 0, 0, 0, 0, 0, 0, 1,
		0, 0, 0, 8, 't', 'r', 'a', 'f',
		0, 0, 0, 12, 'm', 'd', 'a', 't', 1, 2, 3, 4,
	}
	if isEmptyFMP4(real) {
		t.Fatal("a fragment with media bytes was matched as an empty placeholder")
	}
}

// Issue #434 offline replay: an LG webOS fMP4 HLS session with a
// multi-segment GOP must serve the in-GOP placeholder segments as valid
// empty fragments (content-type video/mp4, structurally empty, 200) and a
// ranged read of one must return 206 with the same bytes. The LG decision
// path and start are exercised through the real Decide/ServePlaylist so the
// recorded sequence (decision -> start -> base reads) is reproduced offline.
func TestIssue434LGWebOSBaseReads(t *testing.T) {
	fr := newFakeRunner()
	fr.chunkGOP = 8 * time.Second
	fr.chunkEnd = 24 * time.Second
	m := newTestManager(t, fr, config.Transcode{FastStart: true}, WithBuffering(60*time.Second, 0, 0))
	info := mustParse(t, "mp4_h264_aac.json")
	info.Duration = 24 * time.Second
	d := core.Decision{
		Kind:        core.DecisionTranscode,
		Container:   ContainerFMP4,
		VideoCopy:   true,
		AudioCopy:   false,
		AudioCodec:  "aac",
		AudioStream: 1,
	}
	hs, err := m.StartHLS(t.Context(), core.HLSRequest{
		SessionID: "issue434-lg-webos",
		InputURL:  "http://127.0.0.1:1/coco.mkv",
		Info:      info,
		Decision:  d,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := hs.(*Session)

	// init.mp4 must be read first: it carries the video track id/timescale
	// the placeholders need.
	if r := get(t, s, InitSegment); r.Code != http.StatusOK {
		t.Fatalf("init: %d", r.Code)
	}
	if !s.fmp4TrackKnown || s.fmp4VideoTrack.Timescale == 0 {
		t.Fatalf("session did not learn the video track from init: %+v known=%v", s.fmp4VideoTrack, s.fmp4TrackKnown)
	}

	// The recorded player read base/00000..00003.m4s. The first is the real
	// GOP; the in-GOP ones are conformant empty fragments, not padding.
	seg0 := get(t, s, 0)
	if seg0.Code != http.StatusOK {
		t.Fatalf("segment 0: %d", seg0.Code)
	}
	if ct := seg0.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("segment 0 content-type = %q, want video/mp4", ct)
	}
	for _, n := range []int{1, 2, 3} {
		r := get(t, s, n)
		if r.Code != http.StatusOK {
			t.Fatalf("segment %d: HTTP %d", n, r.Code)
		}
		if ct := r.Header().Get("Content-Type"); ct != "video/mp4" {
			t.Fatalf("segment %d content-type = %q, want video/mp4", n, ct)
		}
		if !isEmptyFMP4(r.Body.Bytes()) {
			t.Fatalf("segment %d is not a conformant empty fragment (%d bytes)", n, r.Body.Len())
		}
	}

	// A ranged read of an in-GOP placeholder returns 206 with its bytes.
	req := httptest.NewRequest(http.MethodGet, "/00003.m4s", nil)
	req.Header.Set("Range", "bytes=8-")
	rec := httptest.NewRecorder()
	s.ServeSegment(rec, req, 3)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("ranged segment 3: HTTP %d, want 206", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), get(t, s, 3).Body.Bytes()[8:]) {
		t.Fatal("ranged bytes differ from the full fragment")
	}
}
