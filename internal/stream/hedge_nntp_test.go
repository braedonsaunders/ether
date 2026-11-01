package stream

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/usenet"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
)

// A viewer read whose head segment stalls on the first provider is answered
// by the fetcher's head-of-line hedge on the second provider after about the
// cold hedge threshold (1s), with production defaults, instead of waiting
// out the 5s idle read deadline. The hedged bytes still pass the engine's
// yEnc range normalisation and reach the reader intact.
func TestLiveReadStalledHeadIsHedged(t *testing.T) {
	want := mkvBytes(31, 600_000)
	first := nntptest.Start(t, nntptest.Options{})
	second := nntptest.Start(t, nntptest.Options{})
	files := map[string][]byte{"video.mkv": want}
	post, err := first.Post(files, 100_000, nntptest.PostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Post(files, 100_000, nntptest.PostOptions{}); err != nil {
		t.Fatal(err)
	}
	head := post.Files[0].Segments[0].MessageID
	first.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" && arg == head {
			return nntptest.Fault{BodyDelay: time.Minute}
		}
		return nntptest.Fault{}
	})
	p, s := first.Provider("first", 4), second.Provider("second", 4)
	s.Priority = 1
	fetcher := usenet.New([]config.UsenetProvider{p, s}, usenet.Options{MinIdle: -1, KeepAlive: -1})
	t.Cleanup(func() { _ = fetcher.Close() })
	e := newTestEngine(t, fetcher, nil, Options{ReadAheadMin: 1, ReadAheadMax: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	media, err := e.Open(ctx, "hedge", post.NZB)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	got := make([]byte, 64<<10)
	if n, err := media.ReadAt(got, 0); err != nil || n != len(got) || !bytes.Equal(got, want[:len(got)]) {
		t.Fatalf("first read: n=%d err=%v", n, err)
	}
	took := time.Since(start)
	t.Logf("time to first byte with the head stalled: %v", took)
	if took > 3*time.Second {
		t.Fatalf("stalled head took %v; the hedge should answer well inside the 5s idle deadline", took)
	}
	if n := second.BodyRequests(head); n != 1 {
		t.Fatalf("head BODYs on the second provider = %d, want one hedge", n)
	}
	var hedges, wins int64
	for _, st := range fetcher.Stats() {
		hedges += st.Hedges
		wins += st.HedgeWins
	}
	if hedges != 1 || wins != 1 {
		t.Fatalf("hedges=%d wins=%d", hedges, wins)
	}
}
