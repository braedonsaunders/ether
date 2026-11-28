package livetv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type archiveTestTranscoder struct{ fakeTranscoder }

func (*archiveTestTranscoder) Probe(context.Context, string) (*core.MediaInfo, error) {
	return &core.MediaInfo{Container: "mpegts", Streams: []core.StreamInfo{{Type: "video", Codec: "h264"}}}, nil
}
func (f *archiveTestTranscoder) StartHLS(ctx context.Context, req core.HLSRequest) (core.HLSSession, error) {
	h, err := f.fakeTranscoder.StartHLS(ctx, req)
	return archiveTestOutput{HLSSession: h}, err
}

type archiveTestOutput struct{ core.HLSSession }

func (archiveTestOutput) HasBufferedPosition(pos time.Duration) bool { return pos < 4*time.Second }

func TestCatchUpSeekAndProviderRefusal(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	var requests atomic.Int32
	var startEpoch atomic.Int64
	var refuse atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		startEpoch.Store(start)
		if refuse.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "test archive data")
	}))
	defer up.Close()
	tc := &archiveTestTranscoder{}
	s := testService(t, Options{Sealer: upgradedSealer(t), Now: func() time.Time { return now }, Transcoder: tc})
	c, err := s.SaveChannel(t.Context(), Channel{ID: "archive", Number: "1", Kind: "iptv", Enabled: true, Name: "Archive fixture",
		StreamURL: up.URL + "/live.ts",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The owner channel API preserves the provider-owned archive record.
	c.Archive = &ChannelArchive{Mode: "default", Days: 2, Source: up.URL + "/archive.ts?start={utc}"}
	if err := s.sealArchive(&c, Channel{}); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.DB.ExecContext(t.Context(), "UPDATE livetv_channels SET data=? WHERE id=?", data, c.ID); err != nil {
		t.Fatal(err)
	}
	user := &core.User{ID: 1, IsOwner: true}
	req := CatchUpRequest{ChannelID: c.ID, Start: now.Add(-time.Minute), Duration: 2 * time.Minute}
	caps := core.ClientCaps{ClientID: "archive-test"}
	info, err := s.StartCatchUp(t.Context(), user, req, caps, "")
	if err != nil {
		t.Fatal(err)
	}
	if !info.End.Equal(now) || len(tc.requests) != 1 || !tc.requests[0].ArchiveInput ||
		strings.Contains(tc.requests[0].InputURL, up.URL) || tc.requests[0].Info.Duration != time.Minute {
		t.Fatal("expected a bounded, relayed archive")
	}
	buffered, err := s.SeekCatchUp(t.Context(), user, info.ID, 2*time.Second)
	if err != nil || buffered.MediaGeneration != info.MediaGeneration || requests.Load() != 1 {
		t.Fatal("buffered seek reopened the provider")
	}
	open := tc.requests[0].OpenArchiveRange
	if open == nil {
		t.Fatal("archive encoder cannot reopen a sought range")
	}
	initial, err := open(t.Context(), 0)
	if err != nil || requests.Load() != 1 {
		t.Fatal("the initial encoder did not share the staged upstream connection")
	}
	initial.Release()
	reopened, err := open(t.Context(), 5*time.Second)
	if err != nil || requests.Load() != 2 || startEpoch.Load() != req.Start.Add(5*time.Second).Unix() {
		t.Fatal("encoder seek did not request programme start plus the segment offset")
	}
	reopened.Release()
	sought, err := s.SeekCatchUp(t.Context(), user, info.ID, 10*time.Second)
	if err != nil || sought.Origin != 10*time.Second || sought.MediaGeneration == info.MediaGeneration || requests.Load() != 3 {
		t.Fatal("unbuffered seek did not open a new range")
	}
	refuse.Store(true)
	_, err = s.SeekCatchUp(t.Context(), user, info.ID, 20*time.Second)
	var unavailable *CatchUpError
	if !errors.As(err, &unavailable) || unavailable.Code != "catchup_unavailable" || requests.Load() != 4 {
		t.Fatal("archive refusal was not mapped")
	}
	refuse.Store(false)
	oldGeneration := tc.requests[1].OpenArchiveRange
	resumed, err := oldGeneration(t.Context(), 0)
	if err != nil || requests.Load() != 5 || startEpoch.Load() != req.Start.Add(10*time.Second).Unix() {
		t.Fatalf("failed seek stranded the current generation: requests=%d, start=%d, err=%v", requests.Load(), startEpoch.Load(), err)
	}
	resumed.Release()
}
