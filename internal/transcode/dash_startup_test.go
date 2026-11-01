package transcode

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestEncodedDASHAdvertisesShortBufferAndServesSeekSegments(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	req := transcodeRequest(t, "short-dash")
	req.Protocol = core.ProtocolDASH
	s := startDASHSession(t, m, req)
	r := httptest.NewRecorder()
	s.ServePlaylist(r, httptest.NewRequest(http.MethodGet, "/start.mpd", nil))
	body := r.Body.String()
	if !strings.Contains(body, `minBufferTime="PT2S"`) || !strings.Contains(body, `duration="2"`) {
		t.Fatalf("encoded DASH retained the long startup buffer: %s", body)
	}
	for _, n := range []int{0, 60, 5} {
		for rep := 0; rep < 2; rep++ {
			if got := media(t, s, rep, n); got.Code != http.StatusOK || got.Body.Len() == 0 {
				t.Fatalf("seek segment %d representation %d: HTTP %d", n, rep, got.Code)
			}
		}
	}
}

func TestDASHRemuxAndExplicitDurationKeepTheirSegmentPlan(t *testing.T) {
	for _, tc := range []struct {
		name           string
		duration, want time.Duration
		encoded        bool
	}{
		{"copy", 0, 5 * time.Second, false},
		{"explicit-copy", 3 * time.Second, 3 * time.Second, false},
		{"explicit-encode", 3 * time.Second, 3 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := newFakeRunner()
			m := newTestManager(t, fr, config.Transcode{})
			req := dashRequest(t, tc.name, time.Minute)
			if tc.encoded {
				req = transcodeRequest(t, tc.name)
				req.Protocol = core.ProtocolDASH
			}
			req.SegmentDuration = tc.duration
			s := startDASHSession(t, m, req)
			if s.segDur != tc.want {
				t.Fatalf("segment duration %v, want %v", s.segDur, tc.want)
			}
		})
	}
}
